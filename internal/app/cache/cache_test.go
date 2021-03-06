package cache

import (
	"testing"
	"time"

	v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	"github.com/golang/groupcache/lru"
	"github.com/golang/protobuf/ptypes/any"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
)

const testKeyA = "key_A"

const testKeyB = "key_B"

type panicValues struct {
	key    lru.Key
	reason string
}

func testOnEvict(key string, value Resource) {
	// TODO: Simulate eviction behavior, e.g. closing of streams.
	panic(panicValues{
		key:    key,
		reason: "testOnEvict called",
	})
}

var testRequestA = v2.DiscoveryRequest{
	VersionInfo: "version_A",
	Node: &core.Node{
		Id:      "id_A",
		Cluster: "cluster_A",
	},
	ResourceNames: []string{"resource_A"},
	TypeUrl:       "typeURL_A",
	ResponseNonce: "nonce_A",
}

var testRequestB = v2.DiscoveryRequest{
	VersionInfo: "version_B",
	Node: &core.Node{
		Id:      "id_B",
		Cluster: "cluster_B",
	},
	ResourceNames: []string{"resource_B"},
	TypeUrl:       "typeURL_B",
	ResponseNonce: "nonce_B",
}

var testDiscoveryResponse = v2.DiscoveryResponse{
	VersionInfo: "version_A",
	Resources: []*any.Any{
		{
			Value: []byte("test"),
		},
	},
	Canary:  false,
	TypeUrl: "typeURL_A",
	Nonce:   "nonce_A",
	ControlPlane: &core.ControlPlane{
		Identifier: "identifier_A",
	},
}

var testResource = Resource{
	Resp:     &testDiscoveryResponse,
	Requests: make(map[*v2.DiscoveryRequest]bool),
}

func TestAddRequestAndFetch(t *testing.T) {
	cache, err := NewCache(1, testOnEvict, time.Second*60)
	assert.NoError(t, err)

	resource, err := cache.Fetch(testKeyA)
	assert.EqualError(t, err, "no value found for key: key_A")
	assert.Nil(t, resource)

	err = cache.AddRequest(testKeyA, &testRequestA)
	assert.NoError(t, err)

	resource, err = cache.Fetch(testKeyA)
	assert.NoError(t, err)
	assert.Nil(t, resource.Resp)
}

func TestSetResponseAndFetch(t *testing.T) {
	cache, err := NewCache(1, testOnEvict, time.Second*60)
	assert.NoError(t, err)

	// Simulate cache miss and setting of new response.
	resource, err := cache.Fetch(testKeyA)
	assert.EqualError(t, err, "no value found for key: key_A")
	assert.Nil(t, resource)

	requests, err := cache.SetResponse(testKeyA, testDiscoveryResponse)
	assert.NoError(t, err)
	assert.Nil(t, requests)

	resource, err = cache.Fetch(testKeyA)
	assert.NoError(t, err)
	assert.Equal(t, testDiscoveryResponse, *resource.Resp)
}

func TestAddRequestAndSetResponse(t *testing.T) {
	cache, err := NewCache(2, testOnEvict, time.Second*60)
	assert.NoError(t, err)

	err = cache.AddRequest(testKeyA, &testRequestA)
	assert.NoError(t, err)

	err = cache.AddRequest(testKeyA, &testRequestB)
	assert.NoError(t, err)

	requests, err := cache.SetResponse(testKeyA, testDiscoveryResponse)
	assert.NoError(t, err)
	assert.Equal(t, 2, len(requests))
	assert.Equal(t, true, requests[&testRequestA])
	assert.Equal(t, true, requests[&testRequestB])

	resource, err := cache.Fetch(testKeyA)
	assert.NoError(t, err)
	assert.Equal(t, testDiscoveryResponse, *resource.Resp)
}

func TestMaxEntries(t *testing.T) {
	cache, err := NewCache(1, testOnEvict, time.Second*60)
	assert.NoError(t, err)

	_, err = cache.SetResponse(testKeyA, testDiscoveryResponse)
	assert.NoError(t, err)

	resource, err := cache.Fetch(testKeyA)
	assert.NoError(t, err)
	assert.Equal(t, testDiscoveryResponse, *resource.Resp)

	assert.PanicsWithValue(t, panicValues{
		key:    testKeyA,
		reason: "testOnEvict called",
	}, func() {
		err = cache.AddRequest(testKeyB, &testRequestB)
		assert.NoError(t, err)
	})

	resource, err = cache.Fetch(testKeyA)
	assert.EqualError(t, err, "no value found for key: key_A")
	assert.Nil(t, resource)

	resource, err = cache.Fetch(testKeyB)
	assert.NoError(t, err)
	assert.Nil(t, resource.Resp)
}

func TestTTL_Enabled(t *testing.T) {
	cache, err := NewCache(1, testOnEvict, time.Millisecond*10)
	assert.NoError(t, err)

	_, err = cache.SetResponse(testKeyA, testDiscoveryResponse)
	assert.NoError(t, err)

	resource, err := cache.Fetch(testKeyA)
	assert.NoError(t, err)
	assert.Equal(t, testDiscoveryResponse, *resource.Resp)

	time.Sleep(time.Millisecond * 10)
	assert.PanicsWithValue(t, panicValues{
		key:    testKeyA,
		reason: "testOnEvict called",
	}, func() {
		resource, err = cache.Fetch(testKeyA)
		assert.NoError(t, err)
		assert.Nil(t, resource)
	})

	resource, err = cache.Fetch(testKeyA)
	assert.EqualError(t, err, "no value found for key: key_A")
	assert.Nil(t, resource)
}

func TestTTL_Disabled(t *testing.T) {
	gomega.RegisterTestingT(t)
	cache, err := NewCache(1, testOnEvict, 0)
	assert.NoError(t, err)

	_, err = cache.SetResponse(testKeyA, testDiscoveryResponse)
	assert.NoError(t, err)

	resource, err := cache.Fetch(testKeyA)
	assert.NoError(t, err)
	assert.Equal(t, testDiscoveryResponse, *resource.Resp)

	gomega.Consistently(func() (*Resource, error) {
		return cache.Fetch(testKeyA)
	}).Should(gomega.Equal(&testResource))
}

func TestTTL_Negative(t *testing.T) {
	cache, err := NewCache(1, testOnEvict, -1)
	assert.EqualError(t, err, "ttl must be nonnegative but was set to -1ns")
	assert.Nil(t, cache)
}

func TestIsExpired(t *testing.T) {
	var resource Resource

	// The expiration time is 0, meaning TTL is disabled, so the resource is not considered expired.
	resource.ExpirationTime = time.Time{}
	assert.False(t, resource.isExpired(time.Now()))

	resource.ExpirationTime = time.Now()
	assert.False(t, resource.isExpired(time.Time{}))

	resource.ExpirationTime = time.Now()
	assert.True(t, resource.isExpired(resource.ExpirationTime.Add(1)))
}

func TestGetExpirationTime(t *testing.T) {
	var c cache

	c.ttl = 0
	assert.Equal(t, time.Time{}, c.getExpirationTime(time.Now()))

	c.ttl = time.Second
	currentTime := time.Date(0, 0, 0, 0, 0, 1, 0, time.UTC)
	expirationTime := time.Date(0, 0, 0, 0, 0, 2, 0, time.UTC)
	assert.Equal(t, expirationTime, c.getExpirationTime(currentTime))
}

func TestDeleteRequest(t *testing.T) {
	cache, err := NewCache(1, testOnEvict, time.Second*60)
	assert.NoError(t, err)

	err = cache.AddRequest(testKeyA, &testRequestA)
	assert.NoError(t, err)

	err = cache.AddRequest(testKeyA, &testRequestA)
	assert.NoError(t, err)

	err = cache.DeleteRequest(testKeyA, &testRequestA)
	assert.NoError(t, err)

	requests, err := cache.SetResponse(testKeyA, testDiscoveryResponse)
	assert.NoError(t, err)
	assert.Equal(t, 0, len(requests))

	err = cache.DeleteRequest(testKeyB, &testRequestB)
	assert.NoError(t, err)
}
