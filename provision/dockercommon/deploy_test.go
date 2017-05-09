// Copyright 2017 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package dockercommon

import (
	"github.com/tsuru/config"
	"gopkg.in/check.v1"
)

func (s *S) TestUserForContainerEmpty(c *check.C) {
	u, uid := UserForContainer()
	c.Assert(u, check.Equals, "")
	c.Assert(uid, check.IsNil)
}

func (s *S) TestUserForContainerOnlyUsername(c *check.C) {
	defer config.Unset("docker:user")
	defer config.Unset("docker:ssh:user")
	config.Set("docker:ssh:user", "iskaralpust")
	u, uid := UserForContainer()
	c.Assert(u, check.Equals, "iskaralpust")
	c.Assert(uid, check.IsNil)
	config.Set("docker:user", "kruppe")
	u, uid = UserForContainer()
	c.Assert(u, check.Equals, "kruppe")
	c.Assert(uid, check.IsNil)
}

func (s *S) TestUserForContainerOnlyUID(c *check.C) {
	config.Set("docker:uid", 1000)
	defer config.Unset("docker:uid")
	u, uid := UserForContainer()
	c.Assert(u, check.Equals, "")
	expectedUid := int64(1000)
	c.Assert(uid, check.NotNil)
	c.Assert(*uid, check.Equals, expectedUid)
}
