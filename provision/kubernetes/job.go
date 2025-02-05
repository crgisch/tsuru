// Copyright 2022 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/tsuru/tsuru/event"
	"github.com/tsuru/tsuru/permission"
	"github.com/tsuru/tsuru/provision"
	permTypes "github.com/tsuru/tsuru/types/permission"
	batchv1 "k8s.io/api/batch/v1"
	apiv1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	jobTypes "github.com/tsuru/tsuru/types/job"
)

func buildJobSpec(job *jobTypes.Job, client *ClusterClient, labels, annotations map[string]string) (batchv1.JobSpec, error) {
	jSpec := job.Spec

	requirements, err := resourceRequirements(job, client, requirementsFactors{})
	if err != nil {
		return batchv1.JobSpec{}, err
	}

	envs := []apiv1.EnvVar{}

	for _, env := range jSpec.Envs {
		envs = append(envs, apiv1.EnvVar{
			Name:  env.Name,
			Value: strings.ReplaceAll(env.Value, "$", "$$"),
		})
	}

	for _, env := range jSpec.ServiceEnvs {
		envs = append(envs, apiv1.EnvVar{
			Name:  env.Name,
			Value: strings.ReplaceAll(env.Value, "$", "$$"),
		})
	}

	imageURL := jSpec.Container.InternalRegistryImage
	if imageURL == "" {
		imageURL = jSpec.Container.OriginalImageSrc
	}

	return batchv1.JobSpec{
		Parallelism:             jSpec.Parallelism,
		BackoffLimit:            jSpec.BackoffLimit,
		Completions:             jSpec.Completions,
		ActiveDeadlineSeconds:   buildActiveDeadline(jSpec.ActiveDeadlineSeconds),
		TTLSecondsAfterFinished: func() *int32 { ttlSecondsAfterFinished := int32(86400); return &ttlSecondsAfterFinished }(), // hardcoded to a day, since we keep logs stored elsewhere on the cloud
		Template: apiv1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      labels,
				Annotations: annotations,
			},
			Spec: apiv1.PodSpec{
				RestartPolicy: "OnFailure",
				Containers: []apiv1.Container{
					{
						Name:      "job",
						Image:     imageURL,
						Command:   jSpec.Container.Command,
						Resources: requirements,
						Env:       envs,
					},
				},
				ServiceAccountName: serviceAccountNameForJob(*job),
			},
		},
	}, nil
}

func buildCronjob(ctx context.Context, client *ClusterClient, job *jobTypes.Job, jobSpec batchv1.JobSpec, labels, annotations map[string]string) (string, error) {
	namespace := client.PoolNamespace(job.Pool)
	k8sCronjob, err := client.BatchV1().CronJobs(namespace).Create(ctx, &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:        job.Name,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: batchv1.CronJobSpec{
			Schedule: job.Spec.Schedule,
			Suspend:  &job.Spec.Manual,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: jobSpec,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	return k8sCronjob.Name, nil
}

func buildMetadata(ctx context.Context, job *jobTypes.Job) (map[string]string, map[string]string) {
	jobLabels := provision.JobLabels(ctx, job).ToLabels()
	customData := job.Metadata
	for _, label := range customData.Labels {
		// don't let custom labels overwrite tsuru labels
		if _, ok := jobLabels[label.Name]; ok {
			continue
		}
		jobLabels[label.Name] = label.Value
	}
	jobAnnotations := map[string]string{}
	for _, a := range job.Metadata.Annotations {
		jobAnnotations[a.Name] = a.Value
	}
	return jobLabels, jobAnnotations
}

func (p *kubernetesProvisioner) CreateJob(ctx context.Context, job *jobTypes.Job) (string, error) {
	client, err := clusterForPool(ctx, job.Pool)
	if err != nil {
		return "", err
	}
	if err = ensureServiceAccountForJob(ctx, client, *job); err != nil {
		return "", err
	}
	jobLabels, jobAnnotations := buildMetadata(ctx, job)
	jobSpec, err := buildJobSpec(job, client, jobLabels, jobAnnotations)
	if err != nil {
		return "", err
	}
	return buildCronjob(ctx, client, job, jobSpec, jobLabels, jobAnnotations)
}

func (p *kubernetesProvisioner) TriggerCron(ctx context.Context, name, pool string) error {
	client, err := clusterForPool(ctx, pool)
	if err != nil {
		return err
	}
	namespace := client.PoolNamespace(pool)
	cron, err := client.BatchV1().CronJobs(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	cronChild := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   cron.Namespace,
			Labels:      cron.Labels,
			Annotations: cron.Annotations,
		},
		Spec: cron.Spec.JobTemplate.Spec,
	}
	cronChild.OwnerReferences = []metav1.OwnerReference{
		{
			Name:       cron.Name,
			Kind:       "CronJob",
			UID:        cron.UID,
			APIVersion: "batch/v1",
		},
	}
	cronChild.Name = getManualJobName(cron.Name)
	if cronChild.Annotations == nil {
		cronChild.Annotations = map[string]string{"cronjob.kubernetes.io/instantiate": "manual"}
	} else {
		cronChild.Annotations["cronjob.kubernetes.io/instantiate"] = "manual"
	}
	_, err = client.BatchV1().Jobs(cron.Namespace).Create(ctx, &cronChild, metav1.CreateOptions{})
	return err
}

func getManualJobName(job string) string {
	scheduledTime := time.Now()
	return fmt.Sprintf("%s-manual-job-%d", job, scheduledTime.Unix()/60)
}

func (p *kubernetesProvisioner) UpdateJob(ctx context.Context, job *jobTypes.Job) error {
	client, err := clusterForPool(ctx, job.Pool)
	if err != nil {
		return err
	}
	if err = ensureServiceAccountForJob(ctx, client, *job); err != nil {
		return err
	}
	jobLabels, jobAnnotations := buildMetadata(ctx, job)
	jobSpec, err := buildJobSpec(job, client, jobLabels, jobAnnotations)
	if err != nil {
		return err
	}
	namespace := client.PoolNamespace(job.Pool)
	_, err = client.BatchV1().CronJobs(namespace).Update(ctx, &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:        job.Name,
			Namespace:   namespace,
			Labels:      jobLabels,
			Annotations: jobAnnotations,
		},
		Spec: batchv1.CronJobSpec{
			Schedule: job.Spec.Schedule,
			Suspend:  &job.Spec.Manual,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: jobSpec,
			},
		},
	}, metav1.UpdateOptions{})
	return err
}

// JobUnits returns information about units related to a specific Job or CronJob
func (p *kubernetesProvisioner) JobUnits(ctx context.Context, job *jobTypes.Job) ([]provision.Unit, error) {
	client, err := clusterForPool(ctx, job.Pool)
	if err != nil {
		return nil, err
	}
	jobLabels := provision.JobLabels(ctx, job).ToLabels()
	labelSelector := metav1.LabelSelector{MatchLabels: jobLabels}
	listOptions := metav1.ListOptions{
		LabelSelector: labels.Set(labelSelector.MatchLabels).String(),
	}
	k8sJobs, err := client.BatchV1().Jobs(client.PoolNamespace(job.Pool)).List(ctx, listOptions)
	if err != nil {
		return nil, err
	}
	return p.jobsToJobUnits(ctx, client, k8sJobs.Items, job)
}

func (p *kubernetesProvisioner) DestroyJob(ctx context.Context, job *jobTypes.Job) error {
	client, err := clusterForPool(ctx, job.Pool)
	if err != nil {
		return err
	}
	namespace := client.PoolNamespace(job.Pool)
	if err := client.CoreV1().ServiceAccounts(namespace).Delete(ctx, serviceAccountNameForJob(*job), metav1.DeleteOptions{}); err != nil && !k8sErrors.IsNotFound(err) {
		return err
	}
	return client.BatchV1().CronJobs(namespace).Delete(ctx, job.Name, metav1.DeleteOptions{})
}

func (p *kubernetesProvisioner) getPodsForJob(ctx context.Context, client *ClusterClient, job *batchv1.Job) ([]apiv1.Pod, error) {
	labelSelector := metav1.LabelSelector{MatchLabels: map[string]string{"job-name": job.Name}}
	listOptions := metav1.ListOptions{
		LabelSelector: labels.Set(labelSelector.MatchLabels).String(),
	}
	pods, err := client.CoreV1().Pods(job.Namespace).List(ctx, listOptions)
	if err != nil {
		return nil, err
	}
	return pods.Items, nil
}

func (p *kubernetesProvisioner) jobsToJobUnits(ctx context.Context, client *ClusterClient, k8sJobs []batchv1.Job, job *jobTypes.Job) ([]provision.Unit, error) {
	if len(k8sJobs) == 0 {
		return nil, nil
	}
	var units []provision.Unit
	for _, k8sJob := range k8sJobs {
		var status provision.Status
		var restarts int32
		pods, err := p.getPodsForJob(ctx, client, &k8sJob)
		if err != nil {
			return nil, err
		}
		for _, pod := range pods {
			restarts += *containersRestarts(pod.Status.ContainerStatuses)
		}
		switch {
		case k8sJob.Status.Failed > 0:
			status = provision.StatusError
		case k8sJob.Status.Succeeded > 0:
			status = provision.StatusSucceeded
		default:
			status = provision.StatusStarted
		}

		createdAt := k8sJob.CreationTimestamp.Time.In(time.UTC)
		units = append(units, provision.Unit{
			ID:        k8sJob.Name,
			Name:      k8sJob.Name,
			Status:    status,
			Restarts:  &restarts,
			CreatedAt: &createdAt,
		})
	}
	return units, nil
}

func createJobEvent(job *batchv1.Job, evt *apiv1.Event) {
	var evtErr error
	var kind *permission.PermissionScheme
	switch evt.Reason {
	case "Completed":
		kind = permission.PermJobRun
	case "BackoffLimitExceeded":
		kind = permission.PermJobRun
		evtErr = errors.New(fmt.Sprintf("job failed: %s", evt.Message))
	case "SuccessfulCreate":
		kind = permission.PermJobCreate
	default:
		return
	}

	realJobOwner := job.Name
	for _, owner := range job.OwnerReferences {
		if owner.Kind == "CronJob" {
			realJobOwner = owner.Name
		}
	}
	opts := event.Opts{
		Kind:       kind,
		Target:     event.Target{Type: event.TargetTypeJob, Value: realJobOwner},
		Allowed:    event.Allowed(permission.PermJobReadEvents, permission.Context(permTypes.CtxJob, realJobOwner)),
		RawOwner:   event.Owner{Type: event.OwnerTypeInternal},
		Cancelable: false,
	}
	e, err := event.New(&opts)
	if err != nil {
		return
	}
	customData := map[string]string{
		"job-name":           job.Name,
		"job-controller":     realJobOwner,
		"event-type":         evt.Type,
		"event-reason":       evt.Reason,
		"message":            evt.Message,
		"cluster-start-time": evt.CreationTimestamp.String(),
	}
	e.DoneCustomData(evtErr, customData)
}

func ensureServiceAccountForJob(ctx context.Context, client *ClusterClient, job jobTypes.Job) error {
	labels := provision.ServiceAccountLabels(provision.ServiceAccountLabelsOpts{
		Job:         &job,
		Provisioner: provisionerName,
		Prefix:      tsuruLabelPrefix,
	})
	ns := client.PoolNamespace(job.Pool)
	return ensureServiceAccount(ctx, client, serviceAccountNameForJob(job), labels, ns, &job.Metadata)
}

func buildActiveDeadline(activeDeadlineSeconds *int64) *int64 {
	defaultActiveDeadline := int64(60 * 60)
	if activeDeadlineSeconds == nil || *activeDeadlineSeconds == int64(0) {
		return &defaultActiveDeadline
	}
	return activeDeadlineSeconds
}
