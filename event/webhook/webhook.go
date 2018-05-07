// Copyright 2018 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tsuru/tsuru/api/shutdown"
	tsuruErrors "github.com/tsuru/tsuru/errors"
	"github.com/tsuru/tsuru/event"
	"github.com/tsuru/tsuru/log"
	tsuruNet "github.com/tsuru/tsuru/net"
	"github.com/tsuru/tsuru/storage"
	eventTypes "github.com/tsuru/tsuru/types/event"
	"github.com/tsuru/tsuru/validation"
)

var (
	_ eventTypes.WebhookService = &webhookService{}

	chanBufferSize   = 1000
	defaultUserAgent = "tsuru-webhook-client/1.0"

	webhooksLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "tsuru_webhooks_latency_seconds",
		Help: "The latency for webhooks requests in seconds",
	})

	webhooksQueue = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "tsuru_webhooks_event_queue_current",
		Help: "The current number of queued events waiting for webhooks processing",
	})

	webhooksTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tsuru_webhooks_calls_total",
		Help: "The total number of webhooks calls",
	})

	webhooksError = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tsuru_webhooks_calls_error",
		Help: "The total number of webhooks calls with error",
	})
)

func init() {
	prometheus.MustRegister(webhooksLatency, webhooksQueue, webhooksTotal, webhooksError)
}

func WebhookService() (eventTypes.WebhookService, error) {
	dbDriver, err := storage.GetCurrentDbDriver()
	if err != nil {
		dbDriver, err = storage.GetDefaultDbDriver()
		if err != nil {
			return nil, err
		}
	}
	s := &webhookService{
		storage: dbDriver.WebhookStorage,
		evtCh:   make(chan string, chanBufferSize),
		quitCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
	go s.run()
	shutdown.Register(s)
	return s, nil
}

type webhookService struct {
	storage eventTypes.WebhookStorage
	evtCh   chan string
	quitCh  chan struct{}
	doneCh  chan struct{}
}

func (s *webhookService) Shutdown(ctx context.Context) error {
	doneCtx := ctx.Done()
	close(s.quitCh)
	select {
	case <-s.doneCh:
	case <-doneCtx:
		return ctx.Err()
	}
	return nil
}

func (s *webhookService) Notify(evtID string) {
	select {
	case s.evtCh <- evtID:
	case <-s.quitCh:
	}
	webhooksQueue.Set(float64(len(s.evtCh)))
}

func (s *webhookService) run() {
	defer close(s.doneCh)
	for {
		select {
		case evtID := <-s.evtCh:
			webhooksQueue.Set(float64(len(s.evtCh)))
			err := s.handleEvent(evtID)
			if err != nil {
				log.Errorf("[webhooks] error handling webhooks for event %q: %v", evtID, err)
			}
		case <-s.quitCh:
			return
		}
	}
}

func (s *webhookService) handleEvent(evtID string) error {
	evt, err := event.GetByHexID(evtID)
	if err != nil {
		return err
	}
	filter := eventTypes.WebhookEventFilter{
		TargetTypes:  []string{string(evt.Target.Type)},
		TargetValues: []string{evt.Target.Value},
		KindTypes:    []string{string(evt.Kind.Type)},
		KindNames:    []string{evt.Kind.Name},
	}
	for _, t := range evt.ExtraTargets {
		filter.TargetTypes = append(filter.TargetTypes, string(t.Target.Type))
		filter.TargetValues = append(filter.TargetValues, t.Target.Value)
	}
	hooks, err := s.storage.FindByEvent(filter, evt.Error == "")
	if err != nil {
		return err
	}
	for _, h := range hooks {
		err = s.doHook(h, evt)
		if err != nil {
			log.Errorf("[webhooks] error calling webhook %q for event %q: %v", h.Name, evtID, err)
		}
	}
	return nil
}

func webhookBody(hook *eventTypes.Webhook, evt *event.Event) (io.Reader, error) {
	if hook.Body != "" {
		return strings.NewReader(hook.Body), nil
	}
	if hook.Method != http.MethodPost &&
		hook.Method != http.MethodPut &&
		hook.Method != http.MethodPatch {
		return nil, nil
	}
	hook.Headers.Set("Content-Type", "application/json")
	data, err := json.Marshal(evt)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(data), nil
}

func (s *webhookService) doHook(hook eventTypes.Webhook, evt *event.Event) (err error) {
	defer func() {
		webhooksTotal.Inc()
		if err != nil {
			webhooksError.Inc()
		}
	}()
	hook.Method = strings.ToUpper(hook.Method)
	if hook.Method == "" {
		hook.Method = http.MethodPost
	}
	body, err := webhookBody(&hook, evt)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(hook.Method, hook.URL, body)
	if err != nil {
		return err
	}
	req.Header = hook.Headers
	if req.UserAgent() == "" {
		req.Header.Set("User-Agent", defaultUserAgent)
	}
	client := tsuruNet.Dial5Full60ClientNoKeepAlive
	if hook.Insecure {
		client = &tsuruNet.Dial5Full60ClientNoKeepAliveInsecure
	}
	reqStart := time.Now()
	rsp, err := client.Do(req)
	webhooksLatency.Observe(time.Since(reqStart).Seconds())
	if err != nil {
		return err
	}
	defer rsp.Body.Close()
	if rsp.StatusCode < 200 || rsp.StatusCode >= 400 {
		data, _ := ioutil.ReadAll(rsp.Body)
		return errors.Errorf("invalid status code calling hook: %d: %s", rsp.StatusCode, string(data))
	}
	return nil
}

func validateURL(u string) error {
	if u == "" {
		return &tsuruErrors.ValidationError{Message: "webhook url must not be empty"}
	}
	_, err := url.Parse(u)
	if err != nil {
		return &tsuruErrors.ValidationError{
			Message: fmt.Sprintf("webhook url is not valid: %v", err),
		}
	}
	return nil
}

func (s *webhookService) Create(w eventTypes.Webhook) error {
	if w.Name == "" {
		return &tsuruErrors.ValidationError{Message: "webhook name must not be empty"}
	}
	err := validation.EnsureValidateName(w.Name)
	if err != nil {
		return err
	}
	err = validateURL(w.URL)
	if err != nil {
		return err
	}
	return s.storage.Insert(w)
}

func (s *webhookService) Update(w eventTypes.Webhook) error {
	err := validateURL(w.URL)
	if err != nil {
		return err
	}
	return s.storage.Update(w)
}

func (s *webhookService) Delete(name string) error {
	return s.storage.Delete(name)
}

func (s *webhookService) Find(name string) (eventTypes.Webhook, error) {
	w, err := s.storage.FindByName(name)
	if err != nil {
		return eventTypes.Webhook{}, err
	}
	return *w, nil
}

func (s *webhookService) List(teams []string) ([]eventTypes.Webhook, error) {
	return s.storage.FindAllByTeams(teams)
}
