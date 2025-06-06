// Copyright 2018-2021 The Cloudprober Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/*
Package client implements a ResourceDiscovery service (RDS) client.
*/
package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/cloudprober/cloudprober/common/iputils"
	"github.com/cloudprober/cloudprober/common/oauth"
	"github.com/cloudprober/cloudprober/common/tlsconfig"
	configpb "github.com/cloudprober/cloudprober/internal/rds/client/proto"
	pb "github.com/cloudprober/cloudprober/internal/rds/proto"
	spb "github.com/cloudprober/cloudprober/internal/rds/proto"
	"github.com/cloudprober/cloudprober/logger"
	"github.com/cloudprober/cloudprober/targets/endpoint"
	dnsRes "github.com/cloudprober/cloudprober/targets/resolver"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	grpcoauth "google.golang.org/grpc/credentials/oauth"
	"google.golang.org/protobuf/proto"
)

// globalResolver is a singleton DNS resolver that is used as the default
// resolver by targets. It is a singleton because dnsRes.Resolver provides a
// cache layer that is best shared by all probes.
var (
	globalResolver dnsRes.Resolver
)

type cacheRecord struct {
	ip          net.IP
	ipStr       string
	port        int
	labels      map[string]string
	lastUpdated time.Time
}

// Default RDS port
const defaultRDSPort = "9314"

// Client represents an RDS based client instance.
type Client struct {
	mu            sync.RWMutex
	c             *configpb.ClientConf
	serverOpts    *configpb.ClientConf_ServerOptions
	dialOpts      []grpc.DialOption
	cache         map[string]*cacheRecord
	names         []string
	listResources func(context.Context, *pb.ListResourcesRequest) (*pb.ListResourcesResponse, error)
	lastModified  int64
	resolver      dnsRes.Resolver
	l             *logger.Logger
}

// ListResourcesFunc is a function that takes ListResourcesRequest and returns
// ListResourcesResponse.
type ListResourcesFunc func(context.Context, *pb.ListResourcesRequest) (*pb.ListResourcesResponse, error)

// refreshState refreshes the client cache.
func (client *Client) refreshState(timeout time.Duration) {
	ctx, cancelFunc := context.WithTimeout(context.Background(), timeout)
	defer cancelFunc()

	req := client.c.GetRequest()
	req.IfModifiedSince = proto.Int64(client.lastModified)

	response, err := client.listResources(ctx, req)
	if err != nil {
		client.l.Errorf("rds.client: error getting resources from RDS server: %v", err)
		return
	}
	client.updateState(response)
}

func parseIP(ipStr string) net.IP {
	if strings.Contains(ipStr, "/") {
		ip, _, err := net.ParseCIDR(ipStr)
		if err != nil {
			return nil
		}
		return ip
	}
	return net.ParseIP(ipStr)
}

func (client *Client) updateState(response *pb.ListResourcesResponse) {
	client.mu.Lock()
	defer client.mu.Unlock()

	// If server doesn't support caching, response's last_modified will be 0 and
	// we'll skip the following block.
	if response.GetLastModified() != 0 && response.GetLastModified() <= client.lastModified {
		// Once we figure out logs throttling story, change this to Info log.
		client.l.Debugf("rds_client: Not refreshing state. Local last-modified: %d, response's last-modified: %d.", client.lastModified, response.GetLastModified())
		return
	}

	client.names = make([]string, len(response.GetResources()))
	oldcache := client.cache
	client.cache = make(map[string]*cacheRecord, len(response.GetResources()))

	i := 0
	for _, res := range response.GetResources() {
		if oldRes, ok := client.cache[res.GetName()]; ok {
			client.l.Warningf("Got resource (%s) again, ignoring this instance: {%v}. Previous record: %+v.", res.GetName(), res, *oldRes)
			continue
		}
		if oldcache[res.GetName()] != nil && res.GetIp() != oldcache[res.GetName()].ipStr {
			client.l.Infof("Resource (%s) ip has changed: %s -> %s.", res.GetName(), oldcache[res.GetName()].ipStr, res.GetIp())
		}

		client.cache[res.GetName()] = &cacheRecord{
			ip:          parseIP(res.GetIp()),
			ipStr:       res.GetIp(),
			port:        int(res.GetPort()),
			labels:      res.Labels,
			lastUpdated: time.Unix(res.GetLastUpdated(), 0),
		}
		client.names[i] = res.GetName()
		i++
	}
	client.names = client.names[:i]
	client.lastModified = response.GetLastModified()
}

// ListEndpoints returns the list of resources.
func (client *Client) ListEndpoints() []endpoint.Endpoint {
	// If ReEvalSec is set to 0 or less, we refresh state on demand.
	if client.c.GetReEvalSec() <= 0 {
		client.refreshState(30 * time.Second)
	}

	client.mu.RLock()
	defer client.mu.RUnlock()
	result := make([]endpoint.Endpoint, len(client.names))
	for i, name := range client.names {
		cr := client.cache[name]
		result[i] = endpoint.Endpoint{Name: name, IP: cr.ip, Port: cr.port, Labels: cr.labels, LastUpdated: cr.lastUpdated}
	}
	return result
}

// Resolve returns the IP address for the given resource. If no IP address is
// associated with the resource, an error is returned.
func (client *Client) Resolve(name string, ipVer int) (net.IP, error) {
	var ip net.IP
	var ipStr string

	client.mu.RLock()
	if cr, ok := client.cache[name]; ok {
		ip, ipStr = cr.ip, cr.ipStr
	}
	client.mu.RUnlock()

	if ipStr == "" {
		return nil, fmt.Errorf("no IP address for the resource: %s", name)
	}

	// If not a valid IP, use DNS resolver to resolve it.
	if ip == nil {
		return client.resolver.Resolve(ipStr, ipVer)
	}

	if ipVer == 0 || iputils.IPVersion(ip) == ipVer {
		return ip, nil
	}

	return nil, fmt.Errorf("no IPv%d address (IP: %s) for %s", ipVer, ip.String(), name)
}

func (client *Client) connect(serverAddr string) (*grpc.ClientConn, error) {
	client.l.Infof("rds.client: using RDS servers at: %s", serverAddr)

	if strings.HasPrefix(serverAddr, "srvlist:///") {
		client.dialOpts = append(client.dialOpts, grpc.WithResolvers(&srvListBuilder{defaultPort: defaultRDSPort}))
	}

	return grpc.Dial(client.serverOpts.GetServerAddress(), client.dialOpts...)
}

// initListResourcesFunc uses server options to establish a connection with the
// given RDS server.
func (client *Client) initListResourcesFunc() error {
	if client.listResources != nil {
		return nil
	}

	if client.serverOpts == nil || client.serverOpts.GetServerAddress() == "" {
		return errors.New("rds.Client: RDS server address not defined")
	}

	// Transport security options.
	if client.serverOpts.GetTlsConfig() != nil {
		tlsConfig := &tls.Config{}
		if err := tlsconfig.UpdateTLSConfig(tlsConfig, client.serverOpts.GetTlsConfig()); err != nil {
			return fmt.Errorf("rds/client: error initializing TLS config (%+v): %v", client.serverOpts.GetTlsConfig(), err)
		}
		client.dialOpts = append(client.dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	} else {
		client.dialOpts = append(client.dialOpts, grpc.WithInsecure())
	}

	// OAuth related options.
	if client.serverOpts.GetOauthConfig() != nil {
		oauthTS, err := oauth.TokenSourceFromConfig(client.serverOpts.GetOauthConfig(), client.l)
		if err != nil {
			return fmt.Errorf("rds/client: error getting token source from OAuth config (%+v): %v", client.serverOpts.GetOauthConfig(), err)
		}
		client.dialOpts = append(client.dialOpts, grpc.WithPerRPCCredentials(grpcoauth.TokenSource{TokenSource: oauthTS}))
	}

	conn, err := client.connect(client.serverOpts.GetServerAddress())
	if err != nil {
		return fmt.Errorf("rds/client: error connecting to server (%v): %v", client.serverOpts.GetServerAddress(), err)
	}

	client.listResources = func(ctx context.Context, in *pb.ListResourcesRequest) (*pb.ListResourcesResponse, error) {
		return spb.NewResourceDiscoveryClient(conn).ListResources(ctx, in)
	}

	return nil
}

// New creates an RDS (ResourceDiscovery service) client instance and set it up
// for continuous refresh.
func New(c *configpb.ClientConf, listResources ListResourcesFunc, l *logger.Logger) (*Client, error) {
	client := &Client{
		c:             c,
		serverOpts:    c.GetServerOptions(),
		cache:         make(map[string]*cacheRecord),
		listResources: listResources,
		resolver:      globalResolver,
		l:             l,
	}

	if err := client.initListResourcesFunc(); err != nil {
		return nil, fmt.Errorf("rds/client: error initializing listListResource function: %v", err)
	}

	if client.c.GetReEvalSec() <= 0 {
		return client, nil
	}

	reEvalInterval := time.Duration(client.c.GetReEvalSec()) * time.Second
	client.refreshState(reEvalInterval)
	go func() {
		// Introduce a random delay between 0-reEvalInterval before starting the
		// refreshState loop. If there are multiple cloudprober instances, this will
		// make sure that each instance calls RDS server at a different point of
		// time.
		rand.Seed(time.Now().UnixNano())
		randomDelaySec := rand.Intn(int(reEvalInterval.Seconds()))
		time.Sleep(time.Duration(randomDelaySec) * time.Second)
		for range time.Tick(reEvalInterval) {
			client.refreshState(reEvalInterval)
		}
	}()

	return client, nil
}

// init initializes the package by creating a new global resolver.
func init() {
	globalResolver = dnsRes.New()
}
