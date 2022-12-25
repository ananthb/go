// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package user

// userdbClient queries the io.systemd.UserDatabase VARLINK interface provided by
// systemd-userdbd.service(8) on Linux for obtaining full user/group details without cgo.
// VARLINK protocol: https://varlink.org
type userdbClient struct {
	perMachineRecord

	serviceSocket string
}

type perMachineRecord struct {
	machineId string
	hostname  string
}
