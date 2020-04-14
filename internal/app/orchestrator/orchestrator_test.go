package orchestrator

import (
	"context"
	"io/ioutil"
	"testing"

	v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	gcp "github.com/envoyproxy/go-control-plane/pkg/cache/v2"
	"github.com/golang/protobuf/ptypes/any"
	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/envoyproxy/xds-relay/internal/app/cache"
	"github.com/envoyproxy/xds-relay/internal/app/mapper"
	"github.com/envoyproxy/xds-relay/internal/app/upstream"
	"github.com/envoyproxy/xds-relay/internal/pkg/log"
	yamlproto "github.com/envoyproxy/xds-relay/internal/pkg/util"
	aggregationv1 "github.com/envoyproxy/xds-relay/pkg/api/aggregation/v1"
)

type mockSimpleUpstreamClient struct {
	responseChan <-chan *upstream.Response
}

func (m mockSimpleUpstreamClient) OpenStream(ctx context.Context, req *v2.DiscoveryRequest) <-chan *upstream.Response {
	return m.responseChan
}

type mockMultiStreamUpstreamClient struct {
	ldsResponseChan <-chan *upstream.Response
	cdsResponseChan <-chan *upstream.Response

	t      *testing.T
	mapper mapper.Mapper
}

func (m mockMultiStreamUpstreamClient) OpenStream(ctx context.Context,
	req *v2.DiscoveryRequest) <-chan *upstream.Response {
	aggregatedKey, err := m.mapper.GetKey(*req)
	assert.NoError(m.t, err)

	if aggregatedKey == "lds" {
		return m.ldsResponseChan
	} else if aggregatedKey == "cds" {
		return m.cdsResponseChan
	}

	m.t.Errorf("Unsupported aggregated key, %s", aggregatedKey)
	return nil
}

func newMockOrchestrator(t *testing.T, mapper mapper.Mapper, upstreamClient upstream.Client) *orchestrator {
	orchestrator := &orchestrator{
		logger:         log.New("info"),
		mapper:         mapper,
		upstreamClient: upstreamClient,
		downstreamResponseMap: downstreamResponseMap{
			responseChannel: make(map[*gcp.Request]chan gcp.Response),
		},
		upstreamResponseMap: upstreamResponseMap{
			responseChannel: make(map[string]upstreamResponseChannel),
		},
	}

	cache, err := cache.NewCache(cacheMaxEntries, orchestrator.onCacheEvicted, cacheTTL)
	assert.NoError(t, err)
	orchestrator.cache = cache

	return orchestrator
}

func newMockMapper(t *testing.T) mapper.Mapper {
	bytes, err := ioutil.ReadFile("testdata/aggregation_rules.yaml") // key on request type
	assert.NoError(t, err)

	var config aggregationv1.KeyerConfiguration
	err = yamlproto.FromYAMLToKeyerConfiguration(string(bytes), &config)
	assert.NoError(t, err)

	return mapper.NewMapper(&config)
}

func assertEqualResources(t *testing.T, got gcp.Response, expected v2.DiscoveryResponse, req gcp.Request) {
	expectedResources, err := cache.MarshalResources(expected.Resources)
	assert.NoError(t, err)
	expectedResponse := cache.Response{
		Raw:                expected,
		MarshaledResources: expectedResources,
	}
	assert.Equal(t, convertToGcpResponse(&expectedResponse, req), got)
}

func TestNew(t *testing.T) {
	// Trivial test to ensure orchestrator instantiates.
	upstreamClient, err := upstream.NewClient(context.Background(), "example.com")
	assert.NoError(t, err)

	config := aggregationv1.KeyerConfiguration{
		Fragments: []*aggregationv1.KeyerConfiguration_Fragment{
			{
				Rules: []*aggregationv1.KeyerConfiguration_Fragment_Rule{},
			},
		},
	}
	requestMapper := mapper.NewMapper(&config)

	orchestrator := New(context.Background(), log.New("info"), requestMapper, upstreamClient)
	assert.NotNil(t, orchestrator)
}

func TestGoldenPath(t *testing.T) {
	upstreamResponseChannel := make(chan *upstream.Response)
	mapper := newMockMapper(t)
	orchestrator := newMockOrchestrator(
		t,
		mapper,
		mockSimpleUpstreamClient{
			responseChan: upstreamResponseChannel,
		},
	)
	assert.NotNil(t, orchestrator)

	req := gcp.Request{
		TypeUrl: "type.googleapis.com/envoy.api.v2.Listener",
	}

	respChannel, cancelWatch := orchestrator.CreateWatch(req)
	assert.NotNil(t, respChannel)
	assert.Equal(t, 1, len(orchestrator.downstreamResponseMap.responseChannel))
	assert.Equal(t, 1, len(orchestrator.upstreamResponseMap.responseChannel))

	upstreamResponse := upstream.Response{
		Response: v2.DiscoveryResponse{
			VersionInfo: "1",
			TypeUrl:     "type.googleapis.com/envoy.api.v2.Listener",
			Resources: []*any.Any{
				&anypb.Any{
					Value: []byte("lds resource"),
				},
			},
		},
	}
	upstreamResponseChannel <- &upstreamResponse

	gotResponse := <-respChannel
	assertEqualResources(t, gotResponse, upstreamResponse.Response, req)

	aggregatedKey, err := mapper.GetKey(req)
	assert.NoError(t, err)
	orchestrator.shutdown(aggregatedKey)
	assert.Equal(t, 0, len(orchestrator.upstreamResponseMap.responseChannel))

	cancelWatch()
	assert.Equal(t, 0, len(orchestrator.downstreamResponseMap.responseChannel))
}

func TestCachedResponse(t *testing.T) {
	upstreamResponseChannel := make(chan *upstream.Response)
	mapper := newMockMapper(t)
	orchestrator := newMockOrchestrator(
		t,
		mapper,
		mockSimpleUpstreamClient{
			responseChan: upstreamResponseChannel,
		},
	)
	assert.NotNil(t, orchestrator)

	// Test scenario with different request and response versions.
	// Version is different, so we expect a response.
	req := gcp.Request{
		VersionInfo: "0",
		TypeUrl:     "type.googleapis.com/envoy.api.v2.Listener",
	}

	aggregatedKey, err := mapper.GetKey(req)
	assert.NoError(t, err)
	mockResponse := v2.DiscoveryResponse{
		VersionInfo: "1",
		TypeUrl:     "type.googleapis.com/envoy.api.v2.Listener",
		Resources: []*any.Any{
			&anypb.Any{
				Value: []byte("lds resource"),
			},
		},
	}
	watches, err := orchestrator.cache.SetResponse(aggregatedKey, mockResponse)
	assert.NoError(t, err)
	assert.Equal(t, 0, len(watches))

	respChannel, cancelWatch := orchestrator.CreateWatch(req)
	assert.NotNil(t, respChannel)
	assert.Equal(t, 1, len(orchestrator.downstreamResponseMap.responseChannel))
	assert.Equal(t, 1, len(orchestrator.upstreamResponseMap.responseChannel))

	gotResponse := <-respChannel
	assertEqualResources(t, gotResponse, mockResponse, req)

	// Attempt pushing a more recent response from upstream.
	upstreamResponse := upstream.Response{
		Response: v2.DiscoveryResponse{
			VersionInfo: "2",
			TypeUrl:     "type.googleapis.com/envoy.api.v2.Listener",
			Resources: []*any.Any{
				&anypb.Any{
					Value: []byte("some other lds resource"),
				},
			},
		},
	}
	upstreamResponseChannel <- &upstreamResponse
	gotResponse = <-respChannel
	assertEqualResources(t, gotResponse, upstreamResponse.Response, req)
	assert.Equal(t, 1, len(orchestrator.upstreamResponseMap.responseChannel))

	// Test scenario with same request and response version.
	// We expect a watch to be open but no response.
	req2 := gcp.Request{
		VersionInfo: "2",
		TypeUrl:     "type.googleapis.com/envoy.api.v2.Listener",
	}

	respChannel2, cancelWatch2 := orchestrator.CreateWatch(req2)
	assert.NotNil(t, respChannel2)
	assert.Equal(t, 2, len(orchestrator.downstreamResponseMap.responseChannel))
	assert.Equal(t, 1, len(orchestrator.upstreamResponseMap.responseChannel))

	// If we pass this point, it's safe to assume the respChannel2 is empty,
	// otherwise the test would block and not complete.
	orchestrator.shutdown(aggregatedKey)
	assert.Equal(t, 0, len(orchestrator.upstreamResponseMap.responseChannel))
	cancelWatch()
	assert.Equal(t, 1, len(orchestrator.downstreamResponseMap.responseChannel))
	cancelWatch2()
	assert.Equal(t, 0, len(orchestrator.downstreamResponseMap.responseChannel))
}

func TestMultipleWatchesAndUpstreams(t *testing.T) {
	upstreamResponseChannelLDS := make(chan *upstream.Response)
	upstreamResponseChannelCDS := make(chan *upstream.Response)
	mapper := newMockMapper(t)
	orchestrator := newMockOrchestrator(
		t,
		mapper,
		mockMultiStreamUpstreamClient{
			ldsResponseChan: upstreamResponseChannelLDS,
			cdsResponseChan: upstreamResponseChannelCDS,
			mapper:          mapper,
			t:               t,
		},
	)
	assert.NotNil(t, orchestrator)

	req1 := gcp.Request{
		TypeUrl: "type.googleapis.com/envoy.api.v2.Listener",
	}
	req2 := gcp.Request{
		TypeUrl: "type.googleapis.com/envoy.api.v2.Listener",
	}
	req3 := gcp.Request{
		TypeUrl: "type.googleapis.com/envoy.api.v2.Cluster",
	}

	respChannel1, cancelWatch1 := orchestrator.CreateWatch(req1)
	assert.NotNil(t, respChannel1)
	respChannel2, cancelWatch2 := orchestrator.CreateWatch(req2)
	assert.NotNil(t, respChannel2)
	respChannel3, cancelWatch3 := orchestrator.CreateWatch(req3)
	assert.NotNil(t, respChannel3)

	upstreamResponseLDS := upstream.Response{
		Response: v2.DiscoveryResponse{
			VersionInfo: "1",
			TypeUrl:     "type.googleapis.com/envoy.api.v2.Listener",
			Resources: []*any.Any{
				&anypb.Any{
					Value: []byte("lds resource"),
				},
			},
		},
	}
	upstreamResponseCDS := upstream.Response{
		Response: v2.DiscoveryResponse{
			VersionInfo: "1",
			TypeUrl:     "type.googleapis.com/envoy.api.v2.Cluster",
			Resources: []*any.Any{
				&anypb.Any{
					Value: []byte("cds resource"),
				},
			},
		},
	}

	upstreamResponseChannelLDS <- &upstreamResponseLDS
	upstreamResponseChannelCDS <- &upstreamResponseCDS

	gotResponseFromChannel1 := <-respChannel1
	gotResponseFromChannel2 := <-respChannel2
	gotResponseFromChannel3 := <-respChannel3

	aggregatedKeyLDS, err := mapper.GetKey(req1)
	assert.NoError(t, err)
	aggregatedKeyCDS, err := mapper.GetKey(req3)
	assert.NoError(t, err)

	assert.Equal(t, 3, len(orchestrator.downstreamResponseMap.responseChannel))
	assert.Equal(t, 2, len(orchestrator.upstreamResponseMap.responseChannel))

	assertEqualResources(t, gotResponseFromChannel1, upstreamResponseLDS.Response, req1)
	assertEqualResources(t, gotResponseFromChannel2, upstreamResponseLDS.Response, req2)
	assertEqualResources(t, gotResponseFromChannel3, upstreamResponseCDS.Response, req3)

	orchestrator.shutdown(aggregatedKeyLDS)
	orchestrator.shutdown(aggregatedKeyCDS)
	assert.Equal(t, 0, len(orchestrator.upstreamResponseMap.responseChannel))

	cancelWatch1()
	cancelWatch2()
	cancelWatch3()
	assert.Equal(t, 0, len(orchestrator.downstreamResponseMap.responseChannel))
}
