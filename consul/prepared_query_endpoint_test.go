package consul

import (
	"fmt"
	"net/rpc"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/consul/consul/structs"
	"github.com/hashicorp/consul/testutil"
	"github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/serf/coordinate"
)

func TestPreparedQuery_Apply(t *testing.T) {
	dir1, s1 := testServer(t)
	defer os.RemoveAll(dir1)
	defer s1.Shutdown()
	codec := rpcClient(t, s1)
	defer codec.Close()

	testutil.WaitForLeader(t, s1.RPC, "dc1")

	// Set up a node and service in the catalog.
	{
		req := structs.RegisterRequest{
			Datacenter: "dc1",
			Node:       "foo",
			Address:    "127.0.0.1",
			Service: &structs.NodeService{
				Service: "redis",
				Tags:    []string{"master"},
				Port:    8000,
			},
		}
		var reply struct{}
		err := msgpackrpc.CallWithCodec(codec, "Catalog.Register", &req, &reply)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
	}

	// Set up a bare bones query.
	query := structs.PreparedQueryRequest{
		Datacenter: "dc1",
		Op:         structs.PreparedQueryCreate,
		Query: &structs.PreparedQuery{
			Service: structs.ServiceQuery{
				Service: "redis",
			},
		},
	}
	var reply string

	// Set an ID which should fail the create.
	query.Query.ID = "nope"
	err := msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply)
	if err == nil || !strings.Contains(err.Error(), "ID must be empty") {
		t.Fatalf("bad: %v", err)
	}

	// Change it to a bogus modify which should also fail.
	query.Op = structs.PreparedQueryUpdate
	query.Query.ID = generateUUID()
	err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply)
	if err == nil || !strings.Contains(err.Error(), "Cannot modify non-existent prepared query") {
		t.Fatalf("bad: %v", err)
	}

	// Fix up the ID but invalidate the query itself. This proves we call
	// parseQuery for a create, but that function is checked in detail as
	// part of another test.
	query.Op = structs.PreparedQueryCreate
	query.Query.ID = ""
	query.Query.Service.Failover.NearestN = -1
	err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply)
	if err == nil || !strings.Contains(err.Error(), "Bad NearestN") {
		t.Fatalf("bad: %v", err)
	}

	// Fix that and make sure it propagates an error from the Raft apply.
	query.Query.Service.Failover.NearestN = 0
	query.Query.Service.Service = "nope"
	err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply)
	if err == nil || !strings.Contains(err.Error(), "invalid service") {
		t.Fatalf("bad: %v", err)
	}

	// Fix that and make sure the apply goes through.
	query.Query.Service.Service = "redis"
	if err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Capture the ID and read the query back to verify.
	query.Query.ID = reply
	{
		req := &structs.PreparedQuerySpecificRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
		}
		var resp structs.IndexedPreparedQueries
		if err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Lookup", req, &resp); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(resp.Queries) != 1 {
			t.Fatalf("bad: %v", resp)
		}
		actual := resp.Queries[0]
		if resp.Index != actual.ModifyIndex {
			t.Fatalf("bad index: %d", resp.Index)
		}
		actual.CreateIndex, actual.ModifyIndex = 0, 0
		if !reflect.DeepEqual(actual, query.Query) {
			t.Fatalf("bad: %v", actual)
		}
	}

	// Make the op an update. This should go through now that we have an ID.
	query.Op = structs.PreparedQueryUpdate
	query.Query.Service.Failover.NearestN = 2
	if err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Read back again to verify the update worked.
	{
		req := &structs.PreparedQuerySpecificRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
		}
		var resp structs.IndexedPreparedQueries
		if err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Lookup", req, &resp); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(resp.Queries) != 1 {
			t.Fatalf("bad: %v", resp)
		}
		actual := resp.Queries[0]
		if resp.Index != actual.ModifyIndex {
			t.Fatalf("bad index: %d", resp.Index)
		}
		actual.CreateIndex, actual.ModifyIndex = 0, 0
		if !reflect.DeepEqual(actual, query.Query) {
			t.Fatalf("bad: %v", actual)
		}
	}

	// Give a bogus op and make sure it fails.
	query.Op = "nope"
	err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply)
	if err == nil || !strings.Contains(err.Error(), "Unknown prepared query operation:") {
		t.Fatalf("bad: %v", err)
	}

	// Prove that an update also goes through the parseQuery validation.
	query.Op = structs.PreparedQueryUpdate
	query.Query.Service.Failover.NearestN = -1
	err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply)
	if err == nil || !strings.Contains(err.Error(), "Bad NearestN") {
		t.Fatalf("bad: %v", err)
	}

	// Now change the op to delete; the bad query field should be ignored
	// because all we care about for a delete op is the ID.
	query.Op = structs.PreparedQueryDelete
	if err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Verify that this query is deleted.
	{
		req := &structs.PreparedQuerySpecificRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
		}
		var resp structs.IndexedPreparedQueries
		if err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Lookup", req, &resp); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(resp.Queries) != 0 {
			t.Fatalf("bad: %v", resp)
		}
	}
}

func TestPreparedQuery_Apply_ACLDeny(t *testing.T) {
	dir1, s1 := testServerWithConfig(t, func(c *Config) {
		c.ACLDatacenter = "dc1"
		c.ACLMasterToken = "root"
		c.ACLDefaultPolicy = "deny"
	})
	defer os.RemoveAll(dir1)
	defer s1.Shutdown()
	codec := rpcClient(t, s1)
	defer codec.Close()

	testutil.WaitForLeader(t, s1.RPC, "dc1")

	// Create two ACLs with read permission to the service.
	var token1, token2 string
	{
		var rules = `
                    service "redis" {
                        policy = "read"
                    }
                `

		req := structs.ACLRequest{
			Datacenter: "dc1",
			Op:         structs.ACLSet,
			ACL: structs.ACL{
				Name:  "User token",
				Type:  structs.ACLTypeClient,
				Rules: rules,
			},
			WriteRequest: structs.WriteRequest{Token: "root"},
		}
		var reply string

		if err := msgpackrpc.CallWithCodec(codec, "ACL.Apply", &req, &reply); err != nil {
			t.Fatalf("err: %v", err)
		}
		token1 = reply

		if err := msgpackrpc.CallWithCodec(codec, "ACL.Apply", &req, &reply); err != nil {
			t.Fatalf("err: %v", err)
		}
		token2 = reply
	}

	// Set up a node and service in the catalog.
	{
		req := structs.RegisterRequest{
			Datacenter: "dc1",
			Node:       "foo",
			Address:    "127.0.0.1",
			Service: &structs.NodeService{
				Service: "redis",
				Tags:    []string{"master"},
				Port:    8000,
			},
			WriteRequest: structs.WriteRequest{Token: "root"},
		}
		var reply struct{}
		err := msgpackrpc.CallWithCodec(codec, "Catalog.Register", &req, &reply)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
	}

	// Set up a bare bones query.
	query := structs.PreparedQueryRequest{
		Datacenter: "dc1",
		Op:         structs.PreparedQueryCreate,
		Query: &structs.PreparedQuery{
			Service: structs.ServiceQuery{
				Service: "redis",
			},
		},
	}
	var reply string

	// Creating without a token should fail since the default policy is to
	// deny.
	err := msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply)
	if err == nil || !strings.Contains(err.Error(), permissionDenied) {
		t.Fatalf("bad: %v", err)
	}

	// Now add the token and try again.
	query.WriteRequest.Token = token1
	if err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Capture the ID and set the token, then read back the query to verify.
	query.Query.ID = reply
	query.Query.Token = token1
	{
		req := &structs.PreparedQuerySpecificRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
			QueryOptions:  structs.QueryOptions{Token: "root"},
		}
		var resp structs.IndexedPreparedQueries
		if err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Lookup", req, &resp); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(resp.Queries) != 1 {
			t.Fatalf("bad: %v", resp)
		}
		actual := resp.Queries[0]
		if resp.Index != actual.ModifyIndex {
			t.Fatalf("bad index: %d", resp.Index)
		}
		actual.CreateIndex, actual.ModifyIndex = 0, 0
		if !reflect.DeepEqual(actual, query.Query) {
			t.Fatalf("bad: %v", actual)
		}
	}

	// Try to do an update with a different token that does have access to
	// the service, but isn't the one that was used to create the query.
	query.Op = structs.PreparedQueryUpdate
	query.WriteRequest.Token = token2
	err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply)
	if err == nil || !strings.Contains(err.Error(), permissionDenied) {
		t.Fatalf("bad: %v", err)
	}

	// Try again with no token.
	query.WriteRequest.Token = ""
	err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply)
	if err == nil || !strings.Contains(err.Error(), permissionDenied) {
		t.Fatalf("bad: %v", err)
	}

	// Try again with the original token. This should go through.
	query.WriteRequest.Token = token1
	if err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Try to do a delete with a different token that does have access to
	// the service, but isn't the one that was used to create the query.
	query.Op = structs.PreparedQueryDelete
	query.WriteRequest.Token = token2
	err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply)
	if err == nil || !strings.Contains(err.Error(), permissionDenied) {
		t.Fatalf("bad: %v", err)
	}

	// Try again with no token.
	query.WriteRequest.Token = ""
	err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply)
	if err == nil || !strings.Contains(err.Error(), permissionDenied) {
		t.Fatalf("bad: %v", err)
	}

	// Try again with the original token. This should go through.
	query.WriteRequest.Token = token1
	if err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Make sure the query got deleted.
	{
		req := &structs.PreparedQuerySpecificRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
			QueryOptions:  structs.QueryOptions{Token: "root"},
		}
		var resp structs.IndexedPreparedQueries
		if err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Lookup", req, &resp); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(resp.Queries) != 0 {
			t.Fatalf("bad: %v", resp)
		}
	}

	// Make the query again.
	query.Op = structs.PreparedQueryCreate
	query.Query.ID = ""
	query.Query.Token = ""
	query.WriteRequest.Token = token1
	if err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Check that it's there.
	query.Query.ID = reply
	query.Query.Token = token1
	{
		req := &structs.PreparedQuerySpecificRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
			QueryOptions:  structs.QueryOptions{Token: "root"},
		}
		var resp structs.IndexedPreparedQueries
		if err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Lookup", req, &resp); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(resp.Queries) != 1 {
			t.Fatalf("bad: %v", resp)
		}
		actual := resp.Queries[0]
		if resp.Index != actual.ModifyIndex {
			t.Fatalf("bad index: %d", resp.Index)
		}
		actual.CreateIndex, actual.ModifyIndex = 0, 0
		if !reflect.DeepEqual(actual, query.Query) {
			t.Fatalf("bad: %v", actual)
		}
	}

	// A management token should be able to update the query no matter what.
	query.Op = structs.PreparedQueryUpdate
	query.WriteRequest.Token = "root"
	if err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply); err != nil {
		t.Fatalf("err: %v", err)
	}

	// That last update should have changed the token to the management one.
	query.Query.Token = "root"
	{
		req := &structs.PreparedQuerySpecificRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
			QueryOptions:  structs.QueryOptions{Token: "root"},
		}
		var resp structs.IndexedPreparedQueries
		if err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Lookup", req, &resp); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(resp.Queries) != 1 {
			t.Fatalf("bad: %v", resp)
		}
		actual := resp.Queries[0]
		if resp.Index != actual.ModifyIndex {
			t.Fatalf("bad index: %d", resp.Index)
		}
		actual.CreateIndex, actual.ModifyIndex = 0, 0
		if !reflect.DeepEqual(actual, query.Query) {
			t.Fatalf("bad: %v", actual)
		}
	}

	// Make another query.
	query.Op = structs.PreparedQueryCreate
	query.Query.ID = ""
	query.Query.Token = ""
	query.WriteRequest.Token = token1
	if err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Check that it's there.
	query.Query.ID = reply
	query.Query.Token = token1
	{
		req := &structs.PreparedQuerySpecificRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
			QueryOptions:  structs.QueryOptions{Token: "root"},
		}
		var resp structs.IndexedPreparedQueries
		if err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Lookup", req, &resp); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(resp.Queries) != 1 {
			t.Fatalf("bad: %v", resp)
		}
		actual := resp.Queries[0]
		if resp.Index != actual.ModifyIndex {
			t.Fatalf("bad index: %d", resp.Index)
		}
		actual.CreateIndex, actual.ModifyIndex = 0, 0
		if !reflect.DeepEqual(actual, query.Query) {
			t.Fatalf("bad: %v", actual)
		}
	}

	// A management token should be able to delete the query no matter what.
	query.Op = structs.PreparedQueryDelete
	query.WriteRequest.Token = "root"
	if err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Make sure the query got deleted.
	{
		req := &structs.PreparedQuerySpecificRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
			QueryOptions:  structs.QueryOptions{Token: "root"},
		}
		var resp structs.IndexedPreparedQueries
		if err = msgpackrpc.CallWithCodec(codec, "PreparedQuery.Lookup", req, &resp); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(resp.Queries) != 0 {
			t.Fatalf("bad: %v", resp)
		}
	}
}

func TestPreparedQuery_Apply_ForwardLeader(t *testing.T) {
	dir1, s1 := testServer(t)
	defer os.RemoveAll(dir1)
	defer s1.Shutdown()
	codec1 := rpcClient(t, s1)
	defer codec1.Close()

	dir2, s2 := testServer(t)
	defer os.RemoveAll(dir2)
	defer s2.Shutdown()
	codec2 := rpcClient(t, s2)
	defer codec2.Close()

	// Try to join.
	addr := fmt.Sprintf("127.0.0.1:%d",
		s1.config.SerfLANConfig.MemberlistConfig.BindPort)
	if _, err := s2.JoinLAN([]string{addr}); err != nil {
		t.Fatalf("err: %v", err)
	}

	testutil.WaitForLeader(t, s1.RPC, "dc1")
	testutil.WaitForLeader(t, s2.RPC, "dc1")

	// Use the follower as the client.
	var codec rpc.ClientCodec
	if !s1.IsLeader() {
		codec = codec1
	} else {
		codec = codec2
	}

	// Set up a node and service in the catalog.
	{
		req := structs.RegisterRequest{
			Datacenter: "dc1",
			Node:       "foo",
			Address:    "127.0.0.1",
			Service: &structs.NodeService{
				Service: "redis",
				Tags:    []string{"master"},
				Port:    8000,
			},
		}
		var reply struct{}
		err := msgpackrpc.CallWithCodec(codec, "Catalog.Register", &req, &reply)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
	}

	// Set up a bare bones query.
	query := structs.PreparedQueryRequest{
		Datacenter: "dc1",
		Op:         structs.PreparedQueryCreate,
		Query: &structs.PreparedQuery{
			Service: structs.ServiceQuery{
				Service: "redis",
			},
		},
	}

	// Make sure the apply works even when forwarded through the non-leader.
	var reply string
	if err := msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply); err != nil {
		t.Fatalf("err: %v", err)
	}
}

func TestPreparedQuery_parseQuery(t *testing.T) {
	query := &structs.PreparedQuery{}

	err := parseQuery(query)
	if err == nil || !strings.Contains(err.Error(), "Must provide a service") {
		t.Fatalf("bad: %v", err)
	}

	query.Service.Service = "foo"
	if err := parseQuery(query); err != nil {
		t.Fatalf("err: %v", err)
	}

	query.Service.Failover.NearestN = -1
	err = parseQuery(query)
	if err == nil || !strings.Contains(err.Error(), "Bad NearestN") {
		t.Fatalf("bad: %v", err)
	}

	query.Service.Failover.NearestN = 3
	if err := parseQuery(query); err != nil {
		t.Fatalf("err: %v", err)
	}

	query.DNS.TTL = "two fortnights"
	err = parseQuery(query)
	if err == nil || !strings.Contains(err.Error(), "Bad DNS TTL") {
		t.Fatalf("bad: %v", err)
	}

	query.DNS.TTL = "-3s"
	err = parseQuery(query)
	if err == nil || !strings.Contains(err.Error(), "must be >=0") {
		t.Fatalf("bad: %v", err)
	}

	query.DNS.TTL = "3s"
	if err := parseQuery(query); err != nil {
		t.Fatalf("err: %v", err)
	}
}

func TestPreparedQuery_Lookup(t *testing.T) {
	dir1, s1 := testServerWithConfig(t, func(c *Config) {
		c.ACLDatacenter = "dc1"
		c.ACLMasterToken = "root"
		c.ACLDefaultPolicy = "deny"
	})
	defer os.RemoveAll(dir1)
	defer s1.Shutdown()
	codec := rpcClient(t, s1)
	defer codec.Close()

	testutil.WaitForLeader(t, s1.RPC, "dc1")

	// Create two ACLs with read permission to the service.
	var token1, token2 string
	{
		var rules = `
                    service "redis" {
                        policy = "read"
                    }
                `

		req := structs.ACLRequest{
			Datacenter: "dc1",
			Op:         structs.ACLSet,
			ACL: structs.ACL{
				Name:  "User token",
				Type:  structs.ACLTypeClient,
				Rules: rules,
			},
			WriteRequest: structs.WriteRequest{Token: "root"},
		}
		var reply string

		if err := msgpackrpc.CallWithCodec(codec, "ACL.Apply", &req, &reply); err != nil {
			t.Fatalf("err: %v", err)
		}
		token1 = reply

		if err := msgpackrpc.CallWithCodec(codec, "ACL.Apply", &req, &reply); err != nil {
			t.Fatalf("err: %v", err)
		}
		token2 = reply
	}

	// Set up a node and service in the catalog.
	{
		req := structs.RegisterRequest{
			Datacenter: "dc1",
			Node:       "foo",
			Address:    "127.0.0.1",
			Service: &structs.NodeService{
				Service: "redis",
				Tags:    []string{"master"},
				Port:    8000,
			},
			WriteRequest: structs.WriteRequest{Token: "root"},
		}
		var reply struct{}
		err := msgpackrpc.CallWithCodec(codec, "Catalog.Register", &req, &reply)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
	}

	// Set up a bare bones query.
	query := structs.PreparedQueryRequest{
		Datacenter: "dc1",
		Op:         structs.PreparedQueryCreate,
		Query: &structs.PreparedQuery{
			Name: "my-query",
			Service: structs.ServiceQuery{
				Service: "redis",
			},
		},
		WriteRequest: structs.WriteRequest{Token: token1},
	}
	var reply string
	if err := msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Capture the ID and set the token, then read back the query to verify.
	query.Query.ID = reply
	query.Query.Token = token1
	{
		req := &structs.PreparedQuerySpecificRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
			QueryOptions:  structs.QueryOptions{Token: token1},
		}
		var resp structs.IndexedPreparedQueries
		if err := msgpackrpc.CallWithCodec(codec, "PreparedQuery.Lookup", req, &resp); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(resp.Queries) != 1 {
			t.Fatalf("bad: %v", resp)
		}
		actual := resp.Queries[0]
		if resp.Index != actual.ModifyIndex {
			t.Fatalf("bad index: %d", resp.Index)
		}
		actual.CreateIndex, actual.ModifyIndex = 0, 0
		if !reflect.DeepEqual(actual, query.Query) {
			t.Fatalf("bad: %v", actual)
		}
	}

	// Now try to read it with a token that has read access to the
	// service but isn't the token used to create the query. This should
	// be denied.
	{
		req := &structs.PreparedQuerySpecificRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
			QueryOptions:  structs.QueryOptions{Token: token2},
		}
		var resp structs.IndexedPreparedQueries
		err := msgpackrpc.CallWithCodec(codec, "PreparedQuery.Lookup", req, &resp)
		if err == nil || !strings.Contains(err.Error(), permissionDenied) {
			t.Fatalf("bad: %v", err)
		}

		if len(resp.Queries) != 0 {
			t.Fatalf("bad: %v", resp)
		}
	}

	// Try again with no token, which should also be denied.
	{
		req := &structs.PreparedQuerySpecificRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
			QueryOptions:  structs.QueryOptions{Token: ""},
		}
		var resp structs.IndexedPreparedQueries
		err := msgpackrpc.CallWithCodec(codec, "PreparedQuery.Lookup", req, &resp)
		if err == nil || !strings.Contains(err.Error(), permissionDenied) {
			t.Fatalf("bad: %v", err)
		}

		if len(resp.Queries) != 0 {
			t.Fatalf("bad: %v", resp)
		}
	}

	// A management token should be able to read no matter what.
	{
		req := &structs.PreparedQuerySpecificRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
			QueryOptions:  structs.QueryOptions{Token: "root"},
		}
		var resp structs.IndexedPreparedQueries
		if err := msgpackrpc.CallWithCodec(codec, "PreparedQuery.Lookup", req, &resp); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(resp.Queries) != 1 {
			t.Fatalf("bad: %v", resp)
		}
		actual := resp.Queries[0]
		if resp.Index != actual.ModifyIndex {
			t.Fatalf("bad index: %d", resp.Index)
		}
		actual.CreateIndex, actual.ModifyIndex = 0, 0
		if !reflect.DeepEqual(actual, query.Query) {
			t.Fatalf("bad: %v", actual)
		}
	}

	// Try a lookup by name instead of ID.
	{
		req := &structs.PreparedQuerySpecificRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.Name,
			QueryOptions:  structs.QueryOptions{Token: token1},
		}
		var resp structs.IndexedPreparedQueries
		if err := msgpackrpc.CallWithCodec(codec, "PreparedQuery.Lookup", req, &resp); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(resp.Queries) != 1 {
			t.Fatalf("bad: %v", resp)
		}
		actual := resp.Queries[0]
		if resp.Index != actual.ModifyIndex {
			t.Fatalf("bad index: %d", resp.Index)
		}
		actual.CreateIndex, actual.ModifyIndex = 0, 0
		if !reflect.DeepEqual(actual, query.Query) {
			t.Fatalf("bad: %v", actual)
		}
	}

	// Try to lookup an unknown ID.
	{
		req := &structs.PreparedQuerySpecificRequest{
			Datacenter:    "dc1",
			QueryIDOrName: generateUUID(),
			QueryOptions:  structs.QueryOptions{Token: token1},
		}
		var resp structs.IndexedPreparedQueries
		if err := msgpackrpc.CallWithCodec(codec, "PreparedQuery.Lookup", req, &resp); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(resp.Queries) != 0 {
			t.Fatalf("bad: %v", resp)
		}
	}

	// Try to lookup an unknown name.
	{
		req := &structs.PreparedQuerySpecificRequest{
			Datacenter:    "dc1",
			QueryIDOrName: "nope",
			QueryOptions:  structs.QueryOptions{Token: token1},
		}
		var resp structs.IndexedPreparedQueries
		if err := msgpackrpc.CallWithCodec(codec, "PreparedQuery.Lookup", req, &resp); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(resp.Queries) != 0 {
			t.Fatalf("bad: %v", resp)
		}
	}
}

func TestPreparedQuery_List(t *testing.T) {
	dir1, s1 := testServerWithConfig(t, func(c *Config) {
		c.ACLDatacenter = "dc1"
		c.ACLMasterToken = "root"
		c.ACLDefaultPolicy = "deny"
	})
	defer os.RemoveAll(dir1)
	defer s1.Shutdown()
	codec := rpcClient(t, s1)
	defer codec.Close()

	testutil.WaitForLeader(t, s1.RPC, "dc1")

	// Create an ACL with read permission to the service.
	var token string
	{
		var rules = `
                    service "redis" {
                        policy = "read"
                    }
                `

		req := structs.ACLRequest{
			Datacenter: "dc1",
			Op:         structs.ACLSet,
			ACL: structs.ACL{
				Name:  "User token",
				Type:  structs.ACLTypeClient,
				Rules: rules,
			},
			WriteRequest: structs.WriteRequest{Token: "root"},
		}
		if err := msgpackrpc.CallWithCodec(codec, "ACL.Apply", &req, &token); err != nil {
			t.Fatalf("err: %v", err)
		}
	}

	// Set up a node and service in the catalog.
	{
		req := structs.RegisterRequest{
			Datacenter: "dc1",
			Node:       "foo",
			Address:    "127.0.0.1",
			Service: &structs.NodeService{
				Service: "redis",
				Tags:    []string{"master"},
				Port:    8000,
			},
			WriteRequest: structs.WriteRequest{Token: "root"},
		}
		var reply struct{}
		err := msgpackrpc.CallWithCodec(codec, "Catalog.Register", &req, &reply)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
	}

	// Query with a legit management token but no queries.
	{
		req := &structs.DCSpecificRequest{
			Datacenter:   "dc1",
			QueryOptions: structs.QueryOptions{Token: "root"},
		}
		var resp structs.IndexedPreparedQueries
		if err := msgpackrpc.CallWithCodec(codec, "PreparedQuery.List", req, &resp); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(resp.Queries) != 0 {
			t.Fatalf("bad: %v", resp)
		}
	}

	// Set up a bare bones query.
	query := structs.PreparedQueryRequest{
		Datacenter: "dc1",
		Op:         structs.PreparedQueryCreate,
		Query: &structs.PreparedQuery{
			Name: "my-query",
			Service: structs.ServiceQuery{
				Service: "redis",
			},
		},
		WriteRequest: structs.WriteRequest{Token: token},
	}
	var reply string
	if err := msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Capture the ID and set the token, then try to list all the queries.
	// A management token is required so this should be denied.
	query.Query.ID = reply
	query.Query.Token = token
	{
		req := &structs.DCSpecificRequest{
			Datacenter:   "dc1",
			QueryOptions: structs.QueryOptions{Token: token},
		}
		var resp structs.IndexedPreparedQueries
		err := msgpackrpc.CallWithCodec(codec, "PreparedQuery.List", req, &resp)
		if err == nil || !strings.Contains(err.Error(), permissionDenied) {
			t.Fatalf("bad: %v", err)
		}

		if len(resp.Queries) != 0 {
			t.Fatalf("bad: %v", resp)
		}
	}

	// An empty token should fail in a similar way.
	{
		req := &structs.DCSpecificRequest{
			Datacenter:   "dc1",
			QueryOptions: structs.QueryOptions{Token: ""},
		}
		var resp structs.IndexedPreparedQueries
		err := msgpackrpc.CallWithCodec(codec, "PreparedQuery.List", req, &resp)
		if err == nil || !strings.Contains(err.Error(), permissionDenied) {
			t.Fatalf("bad: %v", err)
		}

		if len(resp.Queries) != 0 {
			t.Fatalf("bad: %v", resp)
		}
	}

	// Now try a legit management token.
	{
		req := &structs.DCSpecificRequest{
			Datacenter:   "dc1",
			QueryOptions: structs.QueryOptions{Token: "root"},
		}
		var resp structs.IndexedPreparedQueries
		if err := msgpackrpc.CallWithCodec(codec, "PreparedQuery.List", req, &resp); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(resp.Queries) != 1 {
			t.Fatalf("bad: %v", resp)
		}
		actual := resp.Queries[0]
		if resp.Index != actual.ModifyIndex {
			t.Fatalf("bad index: %d", resp.Index)
		}
		actual.CreateIndex, actual.ModifyIndex = 0, 0
		if !reflect.DeepEqual(actual, query.Query) {
			t.Fatalf("bad: %v", actual)
		}
	}
}

// This is a beast of a test, but the setup is so extensive it makes sense to
// walk through the different cases once we have it up. This is broken into
// sections so it's still pretty easy to read.
func TestPreparedQuery_Execute(t *testing.T) {
	dir1, s1 := testServerWithConfig(t, func(c *Config) {
		c.ACLDatacenter = "dc1"
		c.ACLMasterToken = "root"
		c.ACLDefaultPolicy = "deny"
	})
	defer os.RemoveAll(dir1)
	defer s1.Shutdown()
	codec1 := rpcClient(t, s1)
	defer codec1.Close()

	dir2, s2 := testServerWithConfig(t, func(c *Config) {
		c.Datacenter = "dc2"
		c.ACLDatacenter = "dc1"
		c.ACLMasterToken = "root"
		c.ACLDefaultPolicy = "deny"
	})
	defer os.RemoveAll(dir2)
	defer s2.Shutdown()
	codec2 := rpcClient(t, s2)
	defer codec2.Close()

	testutil.WaitForLeader(t, s1.RPC, "dc1")
	testutil.WaitForLeader(t, s2.RPC, "dc2")

	// Try to WAN join.
	addr := fmt.Sprintf("127.0.0.1:%d",
		s1.config.SerfWANConfig.MemberlistConfig.BindPort)
	if _, err := s2.JoinWAN([]string{addr}); err != nil {
		t.Fatalf("err: %v", err)
	}
	testutil.WaitForResult(
		func() (bool, error) {
			return len(s1.WANMembers()) > 1, nil
		},
		func(err error) {
			t.Fatalf("Failed waiting for WAN join: %v", err)
		})

	// Create an ACL with read permission to the service.
	var token string
	{
		var rules = `
                    service "foo" {
                        policy = "read"
                    }
                `

		req := structs.ACLRequest{
			Datacenter: "dc1",
			Op:         structs.ACLSet,
			ACL: structs.ACL{
				Name:  "User token",
				Type:  structs.ACLTypeClient,
				Rules: rules,
			},
			WriteRequest: structs.WriteRequest{Token: "root"},
		}
		if err := msgpackrpc.CallWithCodec(codec1, "ACL.Apply", &req, &token); err != nil {
			t.Fatalf("err: %v", err)
		}
	}

	// Set up some nodes in each DC that host the service.
	{
		for i := 0; i < 10; i++ {
			for _, dc := range []string{"dc1", "dc2"} {
				req := structs.RegisterRequest{
					Datacenter: dc,
					Node:       fmt.Sprintf("node%d", i+1),
					Address:    fmt.Sprintf("127.0.0.%d", i+1),
					Service: &structs.NodeService{
						Service: "foo",
						Port:    8000,
						Tags:    []string{dc, fmt.Sprintf("tag%d", i+1)},
					},
					WriteRequest: structs.WriteRequest{Token: "root"},
				}

				var codec rpc.ClientCodec
				if dc == "dc1" {
					codec = codec1
				} else {
					codec = codec2
				}

				var reply struct{}
				if err := msgpackrpc.CallWithCodec(codec, "Catalog.Register", &req, &reply); err != nil {
					t.Fatalf("err: %v", err)
				}
			}
		}
	}

	// Set up a service query.
	query := structs.PreparedQueryRequest{
		Datacenter: "dc1",
		Op:         structs.PreparedQueryCreate,
		Query: &structs.PreparedQuery{
			Service: structs.ServiceQuery{
				Service: "foo",
			},
			DNS: structs.QueryDNSOptions{
				TTL: "10s",
			},
		},
		WriteRequest: structs.WriteRequest{Token: token},
	}
	if err := msgpackrpc.CallWithCodec(codec1, "PreparedQuery.Apply", &query, &query.Query.ID); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Run a query that doesn't exist.
	{
		req := structs.PreparedQueryExecuteRequest{
			Datacenter:    "dc1",
			QueryIDOrName: "nope",
		}

		var reply structs.PreparedQueryExecuteResponse
		err := msgpackrpc.CallWithCodec(codec1, "PreparedQuery.Execute", &req, &reply)
		if err == nil || !strings.Contains(err.Error(), ErrQueryNotFound.Error()) {
			t.Fatalf("bad: %v", err)
		}

		if len(reply.Nodes) != 0 {
			t.Fatalf("bad: %v", reply)
		}
	}

	// Run the registered query.
	{
		req := structs.PreparedQueryExecuteRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
		}

		var reply structs.PreparedQueryExecuteResponse
		if err := msgpackrpc.CallWithCodec(codec1, "PreparedQuery.Execute", &req, &reply); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(reply.Nodes) != 10 ||
			reply.Datacenter != "dc1" || reply.Failovers != 0 ||
			!reflect.DeepEqual(reply.DNS, query.Query.DNS) {
			t.Fatalf("bad: %v", reply)
		}
	}

	// Try with a limit.
	{
		req := structs.PreparedQueryExecuteRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
			Limit:         3,
		}

		var reply structs.PreparedQueryExecuteResponse
		if err := msgpackrpc.CallWithCodec(codec1, "PreparedQuery.Execute", &req, &reply); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(reply.Nodes) != 3 ||
			reply.Datacenter != "dc1" || reply.Failovers != 0 ||
			!reflect.DeepEqual(reply.DNS, query.Query.DNS) {
			t.Fatalf("bad: %v", reply)
		}
	}

	// Push a coordinate for one of the nodes so we can try an RTT sort. We
	// have to sleep a little while for the coordinate batch to get flushed.
	{
		req := structs.CoordinateUpdateRequest{
			Datacenter: "dc1",
			Node:       "node3",
			Coord:      coordinate.NewCoordinate(coordinate.DefaultConfig()),
		}
		var out struct{}
		if err := msgpackrpc.CallWithCodec(codec1, "Coordinate.Update", &req, &out); err != nil {
			t.Fatalf("err: %v", err)
		}
		time.Sleep(2 * s1.config.CoordinateUpdatePeriod)
	}

	// Try an RTT sort. We don't have any other coordinates in there but
	// showing that the node with a coordinate is always first proves we
	// call the RTT sorting function, which is tested elsewhere.
	for i := 0; i < 100; i++ {
		req := structs.PreparedQueryExecuteRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
			Source: structs.QuerySource{
				Datacenter: "dc1",
				Node:       "node3",
			},
		}

		var reply structs.PreparedQueryExecuteResponse
		if err := msgpackrpc.CallWithCodec(codec1, "PreparedQuery.Execute", &req, &reply); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(reply.Nodes) != 10 ||
			reply.Datacenter != "dc1" || reply.Failovers != 0 ||
			!reflect.DeepEqual(reply.DNS, query.Query.DNS) {
			t.Fatalf("bad: %v", reply)
		}
		if reply.Nodes[0].Node.Node != "node3" {
			t.Fatalf("bad: %v", reply)
		}
	}

	// Make sure the shuffle looks like it's working.
	uniques := make(map[string]struct{})
	for i := 0; i < 100; i++ {
		req := structs.PreparedQueryExecuteRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
		}

		var reply structs.PreparedQueryExecuteResponse
		if err := msgpackrpc.CallWithCodec(codec1, "PreparedQuery.Execute", &req, &reply); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(reply.Nodes) != 10 ||
			reply.Datacenter != "dc1" || reply.Failovers != 0 ||
			!reflect.DeepEqual(reply.DNS, query.Query.DNS) {
			t.Fatalf("bad: %v", reply)
		}
		var names []string
		for _, node := range reply.Nodes {
			names = append(names, node.Node.Node)
		}
		key := strings.Join(names, "|")
		uniques[key] = struct{}{}
	}

	// We have to allow for the fact that there won't always be a unique
	// shuffle each pass, so we just look for smell here without the test
	// being flaky.
	if len(uniques) < 50 {
		t.Fatalf("unique shuffle ratio too low: %d/100", len(uniques))
	}

	// Update the health of a node to mark it critical.
	setHealth := func(node string, health string) {
		req := structs.RegisterRequest{
			Datacenter: "dc1",
			Node:       node,
			Address:    "127.0.0.1",
			Service: &structs.NodeService{
				Service: "foo",
				Port:    8000,
				Tags:    []string{"dc1", "tag1"},
			},
			Check: &structs.HealthCheck{
				Name:      "failing",
				Status:    health,
				ServiceID: "foo",
			},
			WriteRequest: structs.WriteRequest{Token: "root"},
		}
		var reply struct{}
		if err := msgpackrpc.CallWithCodec(codec1, "Catalog.Register", &req, &reply); err != nil {
			t.Fatalf("err: %v", err)
		}
	}
	setHealth("node1", structs.HealthCritical)

	// The failing node should be filtered.
	{
		req := structs.PreparedQueryExecuteRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
		}

		var reply structs.PreparedQueryExecuteResponse
		if err := msgpackrpc.CallWithCodec(codec1, "PreparedQuery.Execute", &req, &reply); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(reply.Nodes) != 9 ||
			reply.Datacenter != "dc1" || reply.Failovers != 0 ||
			!reflect.DeepEqual(reply.DNS, query.Query.DNS) {
			t.Fatalf("bad: %v", reply)
		}
		for _, node := range reply.Nodes {
			if node.Node.Node == "node1" {
				t.Fatalf("bad: %v", node)
			}
		}
	}

	// Upgrade it to a warning and re-query, should be 10 nodes again.
	setHealth("node1", structs.HealthWarning)
	{
		req := structs.PreparedQueryExecuteRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
		}

		var reply structs.PreparedQueryExecuteResponse
		if err := msgpackrpc.CallWithCodec(codec1, "PreparedQuery.Execute", &req, &reply); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(reply.Nodes) != 10 ||
			reply.Datacenter != "dc1" || reply.Failovers != 0 ||
			!reflect.DeepEqual(reply.DNS, query.Query.DNS) {
			t.Fatalf("bad: %v", reply)
		}
	}

	// Make the query more picky so it excludes warning nodes.
	query.Op = structs.PreparedQueryUpdate
	query.Query.Service.OnlyPassing = true
	if err := msgpackrpc.CallWithCodec(codec1, "PreparedQuery.Apply", &query, &query.Query.ID); err != nil {
		t.Fatalf("err: %v", err)
	}

	// The node in the warning state should be filtered.
	{
		req := structs.PreparedQueryExecuteRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
		}

		var reply structs.PreparedQueryExecuteResponse
		if err := msgpackrpc.CallWithCodec(codec1, "PreparedQuery.Execute", &req, &reply); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(reply.Nodes) != 9 ||
			reply.Datacenter != "dc1" || reply.Failovers != 0 ||
			!reflect.DeepEqual(reply.DNS, query.Query.DNS) {
			t.Fatalf("bad: %v", reply)
		}
		for _, node := range reply.Nodes {
			if node.Node.Node == "node1" {
				t.Fatalf("bad: %v", node)
			}
		}
	}

	// Make the query more picky by adding a tag filter. This just proves we
	// call into the tag filter, it is tested more thoroughly in a separate
	// test.
	query.Query.Service.Tags = []string{"!tag3"}
	if err := msgpackrpc.CallWithCodec(codec1, "PreparedQuery.Apply", &query, &query.Query.ID); err != nil {
		t.Fatalf("err: %v", err)
	}

	// The node in the warning state should be filtered as well as the node
	// with the filtered tag.
	{
		req := structs.PreparedQueryExecuteRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
		}

		var reply structs.PreparedQueryExecuteResponse
		if err := msgpackrpc.CallWithCodec(codec1, "PreparedQuery.Execute", &req, &reply); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(reply.Nodes) != 8 ||
			reply.Datacenter != "dc1" || reply.Failovers != 0 ||
			!reflect.DeepEqual(reply.DNS, query.Query.DNS) {
			t.Fatalf("bad: %v", reply)
		}
		for _, node := range reply.Nodes {
			if node.Node.Node == "node1" || node.Node.Node == "node3" {
				t.Fatalf("bad: %v", node)
			}
		}
	}

	// Now fail everything in dc1 and we should get an empty list back.
	for i := 0; i < 10; i++ {
		setHealth(fmt.Sprintf("node%d", i+1), structs.HealthCritical)
	}
	{
		req := structs.PreparedQueryExecuteRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
		}

		var reply structs.PreparedQueryExecuteResponse
		if err := msgpackrpc.CallWithCodec(codec1, "PreparedQuery.Execute", &req, &reply); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(reply.Nodes) != 0 ||
			reply.Datacenter != "dc1" || reply.Failovers != 0 ||
			!reflect.DeepEqual(reply.DNS, query.Query.DNS) {
			t.Fatalf("bad: %v", reply)
		}
	}

	// Modify the query to have it fail over to a bogus DC and then dc2.
	query.Query.Service.Failover.Datacenters = []string{"bogus", "dc2"}
	if err := msgpackrpc.CallWithCodec(codec1, "PreparedQuery.Apply", &query, &query.Query.ID); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Now we should see 9 nodes from dc2 (we have the tag filter still).
	{
		req := structs.PreparedQueryExecuteRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
		}

		var reply structs.PreparedQueryExecuteResponse
		if err := msgpackrpc.CallWithCodec(codec1, "PreparedQuery.Execute", &req, &reply); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(reply.Nodes) != 9 ||
			reply.Datacenter != "dc2" || reply.Failovers != 1 ||
			!reflect.DeepEqual(reply.DNS, query.Query.DNS) {
			t.Fatalf("bad: %v", reply)
		}
		for _, node := range reply.Nodes {
			if node.Node.Node == "node3" {
				t.Fatalf("bad: %v", node)
			}
		}
	}

	// Make sure the limit and query options are forwarded.
	{
		req := structs.PreparedQueryExecuteRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
			Limit:         3,
			QueryOptions:  structs.QueryOptions{RequireConsistent: true},
		}

		var reply structs.PreparedQueryExecuteResponse
		if err := msgpackrpc.CallWithCodec(codec1, "PreparedQuery.Execute", &req, &reply); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(reply.Nodes) != 3 ||
			reply.Datacenter != "dc2" || reply.Failovers != 1 ||
			!reflect.DeepEqual(reply.DNS, query.Query.DNS) {
			t.Fatalf("bad: %v", reply)
		}
		for _, node := range reply.Nodes {
			if node.Node.Node == "node3" {
				t.Fatalf("bad: %v", node)
			}
		}
	}

	// Make sure the remote shuffle looks like it's working.
	uniques = make(map[string]struct{})
	for i := 0; i < 100; i++ {
		req := structs.PreparedQueryExecuteRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
		}

		var reply structs.PreparedQueryExecuteResponse
		if err := msgpackrpc.CallWithCodec(codec1, "PreparedQuery.Execute", &req, &reply); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(reply.Nodes) != 9 ||
			reply.Datacenter != "dc2" || reply.Failovers != 1 ||
			!reflect.DeepEqual(reply.DNS, query.Query.DNS) {
			t.Fatalf("bad: %v", reply)
		}
		var names []string
		for _, node := range reply.Nodes {
			names = append(names, node.Node.Node)
		}
		key := strings.Join(names, "|")
		uniques[key] = struct{}{}
	}

	// We have to allow for the fact that there won't always be a unique
	// shuffle each pass, so we just look for smell here without the test
	// being flaky.
	if len(uniques) < 50 {
		t.Fatalf("unique shuffle ratio too low: %d/100", len(uniques))
	}

	// Finally, take away the token's ability to read the service.
	{
		var rules = `
                    service "foo" {
                        policy = "deny"
                    }
                `

		req := structs.ACLRequest{
			Datacenter: "dc1",
			Op:         structs.ACLSet,
			ACL: structs.ACL{
				ID:    token,
				Name:  "User token",
				Type:  structs.ACLTypeClient,
				Rules: rules,
			},
			WriteRequest: structs.WriteRequest{Token: "root"},
		}
		if err := msgpackrpc.CallWithCodec(codec1, "ACL.Apply", &req, &token); err != nil {
			t.Fatalf("err: %v", err)
		}
	}

	// Now the query should be denied.
	{
		req := structs.PreparedQueryExecuteRequest{
			Datacenter:    "dc1",
			QueryIDOrName: query.Query.ID,
		}

		var reply structs.PreparedQueryExecuteResponse
		err := msgpackrpc.CallWithCodec(codec1, "PreparedQuery.Execute", &req, &reply)
		if err == nil || !strings.Contains(err.Error(), permissionDenied) {
			t.Fatalf("bad: %v", err)
		}

		if len(reply.Nodes) != 0 {
			t.Fatalf("bad: %v", reply)
		}
	}
}

func TestPreparedQuery_Execute_ForwardLeader(t *testing.T) {
	dir1, s1 := testServer(t)
	defer os.RemoveAll(dir1)
	defer s1.Shutdown()
	codec1 := rpcClient(t, s1)
	defer codec1.Close()

	dir2, s2 := testServer(t)
	defer os.RemoveAll(dir2)
	defer s2.Shutdown()
	codec2 := rpcClient(t, s2)
	defer codec2.Close()

	// Try to join.
	addr := fmt.Sprintf("127.0.0.1:%d",
		s1.config.SerfLANConfig.MemberlistConfig.BindPort)
	if _, err := s2.JoinLAN([]string{addr}); err != nil {
		t.Fatalf("err: %v", err)
	}

	testutil.WaitForLeader(t, s1.RPC, "dc1")
	testutil.WaitForLeader(t, s2.RPC, "dc1")

	// Use the follower as the client.
	var codec rpc.ClientCodec
	if !s1.IsLeader() {
		codec = codec1
	} else {
		codec = codec2
	}

	// Set up a node and service in the catalog.
	{
		req := structs.RegisterRequest{
			Datacenter: "dc1",
			Node:       "foo",
			Address:    "127.0.0.1",
			Service: &structs.NodeService{
				Service: "redis",
				Tags:    []string{"master"},
				Port:    8000,
			},
		}
		var reply struct{}
		if err := msgpackrpc.CallWithCodec(codec, "Catalog.Register", &req, &reply); err != nil {
			t.Fatalf("err: %v", err)
		}
	}

	// Set up a bare bones query.
	query := structs.PreparedQueryRequest{
		Datacenter: "dc1",
		Op:         structs.PreparedQueryCreate,
		Query: &structs.PreparedQuery{
			Service: structs.ServiceQuery{
				Service: "redis",
			},
		},
	}
	var reply string
	if err := msgpackrpc.CallWithCodec(codec, "PreparedQuery.Apply", &query, &reply); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Execute it through the follower.
	{
		req := structs.PreparedQueryExecuteRequest{
			Datacenter:    "dc1",
			QueryIDOrName: reply,
		}
		var reply structs.PreparedQueryExecuteResponse
		if err := msgpackrpc.CallWithCodec(codec, "PreparedQuery.Execute", &req, &reply); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(reply.Nodes) != 1 {
			t.Fatalf("bad: %v", reply)
		}
	}

	// Execute it through the follower with consistency turned on.
	{
		req := structs.PreparedQueryExecuteRequest{
			Datacenter:    "dc1",
			QueryIDOrName: reply,
			QueryOptions:  structs.QueryOptions{RequireConsistent: true},
		}
		var reply structs.PreparedQueryExecuteResponse
		if err := msgpackrpc.CallWithCodec(codec, "PreparedQuery.Execute", &req, &reply); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(reply.Nodes) != 1 {
			t.Fatalf("bad: %v", reply)
		}
	}

	// Remote execute it through the follower.
	{
		req := structs.PreparedQueryExecuteRemoteRequest{
			Datacenter: "dc1",
			Query:      *query.Query,
		}
		var reply structs.PreparedQueryExecuteResponse
		if err := msgpackrpc.CallWithCodec(codec, "PreparedQuery.ExecuteRemote", &req, &reply); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(reply.Nodes) != 1 {
			t.Fatalf("bad: %v", reply)
		}
	}

	// Remote execute it through the follower with consistency turned on.
	{
		req := structs.PreparedQueryExecuteRemoteRequest{
			Datacenter:   "dc1",
			Query:        *query.Query,
			QueryOptions: structs.QueryOptions{RequireConsistent: true},
		}
		var reply structs.PreparedQueryExecuteResponse
		if err := msgpackrpc.CallWithCodec(codec, "PreparedQuery.ExecuteRemote", &req, &reply); err != nil {
			t.Fatalf("err: %v", err)
		}

		if len(reply.Nodes) != 1 {
			t.Fatalf("bad: %v", reply)
		}
	}
}
