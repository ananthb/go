// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux

package user

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

const (
	// Systemd userdb VARLINK interface: https://systemd.io/USER_GROUP_API
	userdbMuxSvc    = "io.systemd.Multiplexer"
	userdbMuxSocket = "/run/systemd/userdb/" + userdbMuxSvc

	userdbNamespace = "io.systemd.UserDatabase"

	// io.systemd.UserDatabase VARLINK interface methods.
	mGetGroupRecord = userdbNamespace + ".GetGroupRecord"
	mGetUserRecord  = userdbNamespace + ".GetUserRecord"
	mGetMemberships = userdbNamespace + ".GetMemberships"

	// io.systemd.UserDatabase VARLINK interface errors.
	errNoRecordFound       = userdbNamespace + ".NoRecordFound"
	errServiceNotAvailable = userdbNamespace + ".ServiceNotAvailable"
)

func getUserdbClient() (*userdbClient, bool) {
	if _, err := os.Stat(userdbMuxSocket); err != nil {
		return nil, false
	}

	return &userdbClient{
		perMachineRecord: getMachineRecord(),
		serviceSocket:    userdbMuxSocket,
	}, true
}

func getMachineRecord() perMachineRecord {
	rec := perMachineRecord{}

	hostname, err := os.Hostname()
	if err != nil {
		return rec
	}
	rec.hostname = hostname

	machineId, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return rec
	}
	rec.machineId = strings.TrimSuffix(string(machineId), "\n")

	return rec
}

// userdbCall represents a VARLINK service call sent to systemd-userdb.
// method is the VARLINK method to call.
// parameters are the VARLINK parameters to pass.
// more indicates if more responses are expected.
type userdbCall struct {
	method     string
	parameters callParameters
	more       bool
}

func (u userdbCall) marshalJSON() []byte {
	params := u.parameters.marshalJSON()

	var data bytes.Buffer
	data.WriteString(`{"method":"`)
	data.WriteString(u.method)
	data.WriteString(`","parameters":`)
	data.Write(params)
	if u.more {
		data.WriteString(`,"more":true`)
	}

	data.WriteString(`}`)
	return data.Bytes()
}

type callParameters struct {
	uid       *int64
	userName  string
	gid       *int64
	groupName string
}

func (c callParameters) marshalJSON() []byte {
	var data bytes.Buffer
	data.WriteString(`{"service":"`)
	data.WriteString(userdbMuxSvc)
	data.WriteString(`"`)

	if c.uid != nil {
		data.WriteString(`,"uid":`)
		data.WriteString(strconv.FormatInt(*c.uid, 10))
	}

	if c.userName != "" {
		data.WriteString(`,"userName":"`)
		data.WriteString(c.userName)
		data.WriteString(`"`)
	}

	if c.gid != nil {
		data.WriteString(`,"gid":`)
		data.WriteString(strconv.FormatInt(*c.gid, 10))
	}

	if c.groupName != "" {
		data.WriteString(`,"groupName":"`)
		data.WriteString(c.groupName)
		data.WriteString(`"`)
	}

	data.WriteString(`}`)
	return data.Bytes()
}

type userdbReply struct {
	err        string
	continues  bool
	parameters jsonObject
}

func (u *userdbReply) unmarshal(data []byte) error {
	reply, _, err := parseJSONObject(data)
	if err != nil {
		return err
	}

	if err, ok := jsonObjectGet[string](reply, "error"); ok {
		u.err = err
	}

	if continues, ok := jsonObjectGet[bool](reply, "continues"); ok {
		u.continues = continues
	}

	if p, ok := jsonObjectGet[jsonObject](reply, "parameters"); ok {
		u.parameters = p
	}

	return nil
}

// query calls the io.systemd.UserDatabase VARLINK interface.
// Replies are unmarshaled into the provided unmarshaler.
// Multiple replies can be unmarshaled by setting more to true in the request.
// Replies with io.systemd.UserDatabase.NoRecordFound errors are skipped.
// Other UserDatabase errors are returned as is.
// If the socket does not exist or if reply has the
// `io.systemd.UserDatabase.ServiceNotAvailable` error, the second return value is false
// indicating that the systemd-userdb service is not available.
func (cl userdbClient) query(ctx context.Context, call userdbCall, u userdbParamsUnmarshaler) (bool, error) {
	request := call.marshalJSON()

	sockFd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return false, err
	}
	defer syscall.Close(sockFd)

	if err := syscall.Connect(sockFd, &syscall.SockaddrUnix{Name: cl.serviceSocket}); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, err
		}

		return true, err
	}

	// Null terminate request.
	if request[len(request)-1] != 0 {
		request = append(request, 0)
	}

	// Write request to socket.
	written := 0
	for written < len(request) {
		if err := ctx.Err(); err != nil {
			return true, err
		}

		if n, err := syscall.Write(sockFd, request[written:]); err != nil {
			return true, err
		} else {
			written += n
		}
	}

	// Read response.
	var resp bytes.Buffer
	for {
		if err := ctx.Err(); err != nil {
			return true, err
		}

		buf := make([]byte, 4096)
		if n, err := syscall.Read(sockFd, buf); err != nil {
			return true, err
		} else if n > 0 {
			resp.Write(buf[:n])
			if buf[n-1] == 0 {
				break
			}
		} else {
			// EOF
			break
		}
	}

	if resp.Len() == 0 {
		return true, nil
	}

	buf := resp.Bytes()
	// Remove trailing 0.
	buf = buf[:len(buf)-1]
	// Split into VARLINK messages.
	msgs := bytes.Split(buf, []byte{0})

	var replyParams []jsonObject

	// Parse VARLINK messages.
	for _, m := range msgs {
		var resp userdbReply
		if err := resp.unmarshal(m); err != nil {
			return true, err
		}

		// Handle VARLINK message errors.
		switch e := resp.err; e {
		case "": // No error.
		case errNoRecordFound: // Ignore not found error.
			continue
		case errServiceNotAvailable:
			return false, nil
		default:
			return true, errors.New(e)
		}

		replyParams = append(replyParams, resp.parameters)

		if !resp.continues {
			break
		}
	}

	return true, u.unmarshalParameters(replyParams)
}

// perMachineMatches returns the perMachine matches for the given object.
// The object is expected to be a jsonObject with a systemd-userdb
// user or group record as described in https://systemd.io/USER_RECORD/.
func perMachineMatches(p perMachineRecord, obj jsonObject) []jsonObject {
	var matches []jsonObject

	if perMachine, ok := jsonObjectGet[[]jsonObject](obj, "perMachine"); ok {
		for _, per := range perMachine {
			matchesHost := false

			if mids, ok := jsonObjectGet[[]string](per, "matchMachineId"); ok {
				for _, id := range mids {
					if id == p.machineId {
						matchesHost = true
						break
					}
				}
			}

			if !matchesHost {
				if mid, ok := jsonObjectGet[string](per, "marchMachineId"); ok {
					if mid == p.machineId {
						matchesHost = true
					}
				}
			}

			if !matchesHost {
				if mhs, ok := jsonObjectGet[[]string](per, "matchHostname"); ok {
					for _, mh := range mhs {
						if mh == p.hostname {
							matchesHost = true
							break
						}
					}
				}
			}

			if !matchesHost {
				if mh, ok := jsonObjectGet[string](per, "matchHostname"); ok {
					if mh == p.hostname {
						matchesHost = true
					}
				}
			}

			if matchesHost {
				matches = append(matches, per)
			}
		}
	}

	return matches
}

// machineBOudnRecord returns a machine bound record for the given machine ID.
// The object is expected to be a jsonObject with a systemd-userdb
// user or group record as described in https://systemd.io/USER_RECORD/.
func machineBoundRecord(machineID string, obj jsonObject) (jsonObject, bool) {
	binding, ok := jsonObjectGet[jsonObject](obj, "binding")
	if !ok {
		return nil, false
	}
	return jsonObjectGet[jsonObject](binding, machineID)
}

type groupRecord struct {
	perMachineRecord

	groupName string
	gid       int64
}

func (g *groupRecord) unmarshalParameters(params []jsonObject) error {
	if len(params) != 1 {
		return fmt.Errorf("unexpected userdb reply")
	}

	record, ok := jsonObjectGet[jsonObject](params[0], "record")
	if !ok {
		return fmt.Errorf("missing or invalid record in userdb reply")
	}

	groupName, ok := jsonObjectGet[string](record, "groupName")
	if !ok {
		return fmt.Errorf("missing or invalid groupName in userdb reply")
	}
	g.groupName = groupName

	if gid, ok := jsonObjectGet[int64](record, "gid"); ok {
		g.gid = gid
	}

	for _, match := range perMachineMatches(g.perMachineRecord, record) {
		if gid, ok := jsonObjectGet[int64](match, "gid"); ok {
			g.gid = gid
		}
	}

	if rec, ok := machineBoundRecord(g.machineId, record); ok {
		if gid, ok := jsonObjectGet[int64](rec, "gid"); ok {
			g.gid = gid
		}
	}

	return nil
}

// queryGroupDb queries the userdb interface for a gid, groupname, or both.
func (cl userdbClient) queryGroupDb(ctx context.Context, gid *int64, groupname string) (*Group, bool, error) {
	group := groupRecord{}
	request := userdbCall{
		method:     mGetGroupRecord,
		parameters: callParameters{gid: gid, groupName: groupname},
	}
	if ok, err := cl.query(ctx, request, &group); !ok || err != nil {
		return nil, ok, fmt.Errorf("error querying systemd-userdb group record: %s", err)
	}
	return &Group{
		Name: group.groupName,
		Gid:  strconv.FormatInt(group.gid, 10),
	}, true, nil
}

type userRecord struct {
	perMachineRecord

	userName      string
	realName      string
	uid           int64
	gid           int64
	homeDirectory string
}

func (u *userRecord) unmarshalParameters(params []jsonObject) error {
	if len(params) != 1 {
		return fmt.Errorf("unexpected userdb reply")
	}

	record, ok := jsonObjectGet[jsonObject](params[0], "record")
	if !ok {
		return fmt.Errorf("missing or invalid record in userdb reply")
	}

	userName, ok := jsonObjectGet[string](record, "userName")
	if !ok {
		return fmt.Errorf("missing or invalid userName in userdb reply")
	}
	u.userName = userName

	if realName, ok := jsonObjectGet[string](record, "realName"); ok {
		u.realName = realName
	}
	if uid, ok := jsonObjectGet[int64](record, "uid"); ok {
		u.uid = uid
	}
	if gid, ok := jsonObjectGet[int64](record, "gid"); ok {
		u.gid = gid
	}
	if homeDirectory, ok := jsonObjectGet[string](record, "homeDirectory"); ok {
		u.homeDirectory = homeDirectory
	}

	for _, match := range perMachineMatches(u.perMachineRecord, record) {
		if realName, ok := jsonObjectGet[string](match, "realName"); ok {
			u.realName = realName
		}
		if uid, ok := jsonObjectGet[int64](match, "uid"); ok {
			u.uid = uid
		}
		if gid, ok := jsonObjectGet[int64](match, "gid"); ok {
			u.gid = gid
		}
		if homeDirectory, ok := jsonObjectGet[string](match, "homeDirectory"); ok {
			u.homeDirectory = homeDirectory
		}
	}

	if rec, ok := machineBoundRecord(u.machineId, record); ok {
		if realName, ok := jsonObjectGet[string](rec, "realName"); ok {
			u.realName = realName
		}
		if uid, ok := jsonObjectGet[int64](rec, "uid"); ok {
			u.uid = uid
		}
		if gid, ok := jsonObjectGet[int64](rec, "gid"); ok {
			u.gid = gid
		}
		if homeDirectory, ok := jsonObjectGet[string](rec, "homeDirectory"); ok {
			u.homeDirectory = homeDirectory
		}
	}

	return nil
}

// queryUserDb queries the userdb interface for a uid, username, or both.
func (cl userdbClient) queryUserDb(ctx context.Context, uid *int64, username string) (*User, bool, error) {
	user := userRecord{}
	request := userdbCall{
		method: mGetUserRecord,
		parameters: callParameters{
			uid:      uid,
			userName: username,
		},
	}

	if ok, err := cl.query(ctx, request, &user); !ok || err != nil {
		return nil, ok, fmt.Errorf("error querying systemd-userdb user record: %s", err)
	}
	return &User{
		Uid:      strconv.FormatInt(user.uid, 10),
		Gid:      strconv.FormatInt(user.gid, 10),
		Username: user.userName,
		Name:     user.realName,
		HomeDir:  user.homeDirectory,
	}, true, nil
}

func (cl userdbClient) lookupGroup(ctx context.Context, groupname string) (*Group, bool, error) {
	return cl.queryGroupDb(ctx, nil, groupname)
}

func (cl userdbClient) lookupGroupId(ctx context.Context, id string) (*Group, bool, error) {
	gid, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, true, err
	}
	return cl.queryGroupDb(ctx, &gid, "")
}

func (cl userdbClient) lookupUser(ctx context.Context, username string) (*User, bool, error) {
	return cl.queryUserDb(ctx, nil, username)
}

func (cl userdbClient) lookupUserId(ctx context.Context, id string) (*User, bool, error) {
	uid, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, true, err
	}
	return cl.queryUserDb(ctx, &uid, "")
}

type memberships struct {
	// Keys are groupNames and values are sets of userNames.
	groupUsers map[string]map[string]struct{}
}

// unmarshalParameters expects many (userName, groupName) record response parameters.
// This is used to build a membership map.
func (m *memberships) unmarshalParameters(params []jsonObject) error {
	m.groupUsers = make(map[string]map[string]struct{})
	// Split records by null terminator.
	for _, ps := range params {
		userName, ok := jsonObjectGet[string](ps, "userName")
		if !ok {
			return fmt.Errorf("missing or invalid userName in userdb reply")
		}
		groupName, ok := jsonObjectGet[string](ps, "groupName")
		if !ok {
			return fmt.Errorf("missing or invalid groupName in userdb reply")
		}

		if _, ok := m.groupUsers[groupName]; ok {
			m.groupUsers[groupName][userName] = struct{}{}
		} else {
			m.groupUsers[groupName] = map[string]struct{}{userName: {}}
		}
	}

	return nil
}

func (cl userdbClient) lookupGroupIds(ctx context.Context, username string) ([]string, bool, error) {
	// Fetch group memberships for username.
	var ms memberships
	request := userdbCall{
		method:     mGetMemberships,
		parameters: callParameters{userName: username},
		more:       true,
	}
	if ok, err := cl.query(ctx, request, &ms); !ok || err != nil {
		return nil, ok, fmt.Errorf("error querying systemd-userdb memberships record: %s", err)
	}

	// Fetch user group gid.
	var group groupRecord
	request = userdbCall{
		method:     mGetGroupRecord,
		parameters: callParameters{groupName: username},
	}
	if ok, err := cl.query(ctx, request, &group); !ok || err != nil {
		return nil, ok, err
	}
	gids := []string{strconv.FormatInt(group.gid, 10)}

	// Fetch group records for each group.
	for g := range ms.groupUsers {
		var group groupRecord
		request.parameters.groupName = g
		// Query group for gid.
		if ok, err := cl.query(ctx, request, &group); !ok || err != nil {
			return nil, ok, fmt.Errorf("error querying systemd-userdb group record: %s", err)
		}
		gids = append(gids, strconv.FormatInt(group.gid, 10))
	}
	return gids, true, nil
}
