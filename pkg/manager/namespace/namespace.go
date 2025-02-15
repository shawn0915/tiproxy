// Copyright 2023 PingCAP, Inc.
// SPDX-License-Identifier: Apache-2.0

// Copyright 2020 Ipalfish, Inc.
// SPDX-License-Identifier: Apache-2.0

package namespace

import (
	"github.com/pingcap/TiProxy/pkg/manager/router"
)

type Namespace struct {
	name   string
	user   string
	router router.Router
}

func (n *Namespace) Name() string {
	return n.name
}

func (n *Namespace) User() string {
	return n.user
}

func (n *Namespace) GetRouter() router.Router {
	return n.router
}

func (n *Namespace) Close() {
	n.router.Close()
}
