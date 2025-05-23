// Copyright 2018 The Cloudprober Authors.
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
Package udplistener implements a UDP listener. Given a target list, it listens
for packets from each of the targets and reports number of packets successfully
received in order, lost or delayed. It also uses the probe interval as an
indicator for the number of packets we expect from each target. Use the "udp"
probe as the counterpart with the same targets list and probe interval as the
sender.

Notes:

Each probe has 3 goroutines:

- A recvLoop that keeps handling incoming packets and updates metrics.

- An outputLoop that ticks twice every statsExportInterval and outputs metrics.

- An echoLoop that receives incoming packets from recvLoop over a channel and
echos back the packets.

- Targets list determines which packet sources are valid sources. It is
updated in the outputLoop routine.

- We use the probe interval to determine the estimated number of packets that
should be received. This number is the lower bound of the total number of
packets "sent" by each source.
*/
package udplistener

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudprober/cloudprober/internal/udpmessage"
	"github.com/cloudprober/cloudprober/logger"
	"github.com/cloudprober/cloudprober/metrics"
	"github.com/cloudprober/cloudprober/probes/options"
	"github.com/cloudprober/cloudprober/targets/endpoint"

	udpsrv "github.com/cloudprober/cloudprober/internal/servers/udp"
	configpb "github.com/cloudprober/cloudprober/probes/udplistener/proto"
)

const (
	maxMsgSize           = 65536
	maxTargets           = 1024
	logThrottleThreshold = 10
)

// Probe holds aggregate information about all probe runs.
type Probe struct {
	name     string
	opts     *options.Options
	c        *configpb.ProbeConf
	l        *logger.Logger
	conn     *net.UDPConn
	echoMode bool

	// map target name to flow state.
	targets []endpoint.Endpoint
	fsm     *udpmessage.FlowStateMap

	// Process and output results synchronization.
	mu   sync.Mutex
	errs *probeErr
	res  map[string]*probeRunResult
}

// proberErr stores error stats and counters for throttled logging.
type probeErr struct {
	throttleCt     int32
	invalidMsgErrs map[string]string // addr -> error string
	missingTargets map[string]int    // sender -> count
}

// echoMsg is a struct that is passed between rx thread and echo thread.
type echoMsg struct {
	addr   *net.UDPAddr
	bufLen int
	buf    []byte
}

func (p *Probe) logErrs() {
	// atomic inc throttleCt so that we don't grab p.mu.Lock() when not logging.
	newVal := atomic.AddInt32(&p.errs.throttleCt, 1)
	if newVal != int32(logThrottleThreshold) {
		return
	}
	defer atomic.StoreInt32(&p.errs.throttleCt, 0)

	p.mu.Lock()
	defer p.mu.Unlock()

	pe := p.errs
	if len(pe.invalidMsgErrs) > 0 {
		p.l.Warningf("Invalid messages received: %v", pe.invalidMsgErrs)
		pe.invalidMsgErrs = make(map[string]string)
	}
	if len(pe.missingTargets) > 0 {
		p.l.Warningf("Unknown targets sending messages: %v", pe.missingTargets)
		pe.missingTargets = make(map[string]int)
	}
}

// probeRunResult captures the results of a single probe run. The way we work with
// stats makes sure that probeRunResult and its fields are not accessed concurrently
// (see documentation with statsKeeper below). That's the reason we use metrics.Int
// types instead of metrics.AtomicInt.
type probeRunResult struct {
	target  string
	total   metrics.Int
	success metrics.Int
	ipdUS   metrics.Int // inter-packet distance in microseconds
	lost    metrics.Int // lost += (currSeq - prevSeq - 1)
	delayed metrics.Int // delayed += (currSeq < prevSeq)
}

// Target returns the p.target.
func (prr probeRunResult) Target() string {
	return prr.target
}

// Metrics converts probeRunResult into metrics.EventMetrics object
func (prr probeRunResult) Metrics() *metrics.EventMetrics {
	return metrics.NewEventMetrics(time.Now()).
		AddMetric("total", &prr.total).
		AddMetric("success", &prr.success).
		AddMetric("ipd_us", &prr.ipdUS).
		AddMetric("lost", &prr.lost).
		AddMetric("delayed", &prr.delayed)
}

func (p *Probe) updateTargets() {
	p.targets = p.opts.Targets.ListEndpoints()

	for _, target := range p.targets {
		for _, al := range p.opts.AdditionalLabels {
			al.UpdateForTarget(target, "", 0)
		}
	}
}

// Init initializes the probe with the given params.
func (p *Probe) Init(name string, opts *options.Options) error {
	c, ok := opts.ProbeConf.(*configpb.ProbeConf)
	if !ok {
		return fmt.Errorf("not a UDP Listener config: %v", opts.ProbeConf)
	}
	p.name = name
	p.opts = opts
	if p.l = opts.Logger; p.l == nil {
		p.l = &logger.Logger{}
	}
	p.c = c
	if p.c == nil {
		p.c = &configpb.ProbeConf{}
	}
	p.echoMode = p.c.GetType() == configpb.ProbeConf_ECHO

	p.fsm = udpmessage.NewFlowStateMap()

	udpAddr := &net.UDPAddr{Port: int(p.c.GetPort())}
	if p.opts.SourceIP != nil {
		udpAddr.IP = p.opts.SourceIP
	}

	conn, err := udpsrv.Listen(udpAddr, p.l)
	if err != nil {
		p.l.Warningf("Opening a listen UDP socket on port %d failed: %v", p.c.GetPort(), err)
		return err
	}
	p.conn = conn

	p.res = make(map[string]*probeRunResult)
	p.errs = &probeErr{
		invalidMsgErrs: make(map[string]string),
		missingTargets: make(map[string]int),
	}
	return nil
}

// cleanup closes the udp socket
func (p *Probe) cleanup() {
	if p.conn != nil {
		p.conn.Close()
	}
}

// initProbeRunResults empties the current probe results objects, updates the
// list of targets and builds a new result object for each target.
func (p *Probe) initProbeRunResults() {
	p.updateTargets()
	if p.echoMode && len(p.targets) > maxTargets {
		p.l.Warningf("too many targets (got %d > max %d), responses might be slow.", len(p.targets), maxTargets)
	}

	p.res = make(map[string]*probeRunResult)
	for _, target := range p.targets {
		p.res[target.Name] = &probeRunResult{
			target: target.Name,
		}
	}
}

// processMessage processes an incoming message and updates metrics.
func (p *Probe) processMessage(buf []byte, rxTS time.Time, srcAddr *net.UDPAddr) {
	p.mu.Lock()
	defer p.mu.Unlock()

	msg, err := udpmessage.NewMessage(buf)
	if err != nil {
		p.errs.invalidMsgErrs[srcAddr.String()] = err.Error()
		return
	}
	src := msg.Src()
	probeRes, ok := p.res[src]
	if !ok {
		p.errs.missingTargets[src]++
		return
	}

	msgRes := msg.ProcessOneWay(p.fsm, rxTS)
	probeRes.total.Inc()
	if msgRes.Success {
		probeRes.success.Inc()
		probeRes.ipdUS.IncBy(msgRes.InterPktDelay.Nanoseconds() / 1000)
	} else if msgRes.LostCount > 0 {
		probeRes.lost.IncBy(int64(msgRes.LostCount))
	} else if msgRes.Delayed {
		probeRes.delayed.Inc()
	}
}

// outputResults writes results to the output channel.
func (p *Probe) outputResults(expectedCt int64, stats chan<- *probeRunResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.res {
		delta := expectedCt - r.total.Int64()
		if delta > 0 {
			r.total.IncBy(delta)
		}
		stats <- r
	}
	p.initProbeRunResults()
}

func (p *Probe) outputLoop(ctx context.Context, stats chan<- *probeRunResult) {
	// Use a ticker to control stats output and error logging.
	// ticker should be a multiple of interval between pkts (i.e., p.opts.Interval).
	pktsPerExportInterval := int64(p.opts.StatsExportInterval / p.opts.Interval)
	tick := p.opts.Interval
	if pktsPerExportInterval > 1 {
		tick = (p.opts.StatsExportInterval / 2).Round(p.opts.Interval)
	}
	ticker := time.NewTicker(tick)

	// #packets-in-an-interval = #sending-ports * (timeDelta + interval - 1ns) / interval
	// We add (interval/2 - 1ns) because int64 takes the floor, whereas we want
	// to round the expression.
	lastExport := time.Now()
	roundAdd := p.opts.Interval/2 - time.Nanosecond
	for {
		select {
		case <-ctx.Done():
			ticker.Stop()
			return
		case <-ticker.C:
			// Number of probes received from a single sender should equal the number of
			// sending intervals in the period times the number of sending ports.
			numIntervals := int64((time.Since(lastExport) + roundAdd) / p.opts.Interval)
			expectedCt := numIntervals * int64(p.c.GetPacketsPerProbe())
			p.outputResults(expectedCt, stats)
			p.logErrs()
			lastExport = time.Now()
		}
	}
}

// echoLoop transmits packets received in the msgChan.
func (p *Probe) echoLoop(ctx context.Context, msgChan chan *echoMsg) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-msgChan:
			n, err := p.conn.WriteToUDP(msg.buf, msg.addr)
			if err == io.EOF { // socket closed. exit the loop.
				return
			}
			if err != nil {
				p.l.Errorf("Error writing echo response to %v: %v", msg.addr, err)
			} else if n < msg.bufLen {
				p.l.Warningf("Reply truncated: sent %d out of %d bytes to %v.", n, msg.bufLen, msg.addr)
			}
		}
	}
}

// recvLoop loops over the listener socket for incoming messages and update stats.
// TODO: Move processMessage to the outputLoop and remove probe mutex.
func (p *Probe) recvLoop(ctx context.Context, echoChan chan<- *echoMsg) {
	conn := p.conn
	// Accommodate the largest UDP message.
	b := make([]byte, maxMsgSize)

	p.initProbeRunResults()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		conn.SetReadDeadline(time.Now().Add(time.Second))
		n, srcAddr, err := conn.ReadFromUDP(b)
		if err != nil {
			p.l.Debugf("Error receiving on UDP socket: %v", err)
			continue
		}
		rxTS := time.Now()
		if p.echoMode {
			e := &echoMsg{
				buf:  make([]byte, n),
				addr: srcAddr,
			}
			copy(e.buf, b[:n])
			echoChan <- e
		}
		p.processMessage(b[:n], rxTS, srcAddr)
	}
}

// probeLoop starts the necessary threads and waits for them to exit.
func (p *Probe) probeLoop(ctx context.Context, resultsChan chan<- *probeRunResult) {
	var wg sync.WaitGroup

	// Output Loop for metrics
	wg.Add(1)
	go func() {
		p.outputLoop(ctx, resultsChan)
		wg.Done()
	}()

	// Echo loop to respond to incoming messages in echo mode.
	var echoChan chan *echoMsg
	if p.echoMode {
		echoChan = make(chan *echoMsg, maxTargets)
		wg.Add(1)
		go func() {
			p.echoLoop(ctx, echoChan)
			wg.Done()
		}()
	}

	p.recvLoop(ctx, echoChan)
	wg.Wait()
}

// statsKeeper manages and outputs probe results.
// TODO: We should get rid of this function. For now, I've copied it from
// common/statskeeper so that we can delete the common package.
func (p *Probe) statsKeeper(ctx context.Context, ptype, name string, opts *options.Options, resultsChan <-chan *probeRunResult, dataChan chan<- *metrics.EventMetrics) {
	targetMetrics := make(map[string]*metrics.EventMetrics)
	exportTicker := time.NewTicker(opts.StatsExportInterval)
	defer exportTicker.Stop()

	for {
		select {
		case result := <-resultsChan:
			// result is a ProbeResult
			t := result.Target()
			if targetMetrics[t] == nil {
				targetMetrics[t] = result.Metrics()
				continue
			}
			em := result.Metrics()
			for _, k := range em.MetricsKeys() {
				if targetMetrics[t].Metric(k) == nil {
					targetMetrics[t].AddMetric(k, em.Metric(k))
				} else {
					if err := targetMetrics[t].Metric(k).Add(em.Metric(k)); err != nil {
						opts.Logger.Errorf("Error adding metric %s for the target: %s. Err: %v", k, t, err)
					}
				}
			}
		case ts := <-exportTicker.C:
			for _, t := range p.targets {
				em := targetMetrics[t.Name]
				if em != nil {
					em.AddLabel("ptype", ptype)
					em.AddLabel("probe", name)
					em.AddLabel("dst", t.Name)
					em.Timestamp = ts

					opts.RecordMetrics(t, em.Clone(), dataChan)
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

// Start starts and runs the probe indefinitely.
func (p *Probe) Start(ctx context.Context, dataChan chan *metrics.EventMetrics) {
	p.updateTargets()

	// Make sure we don't create zero length results channel.
	minResultsChLen := 10
	resultsChLen := len(p.targets)
	if resultsChLen < minResultsChLen {
		resultsChLen = minResultsChLen
	}
	resultsChan := make(chan *probeRunResult, resultsChLen)

	go p.statsKeeper(ctx, "udp", p.name, p.opts, resultsChan, dataChan)

	// probeLoop runs forever and returns only when the probe has to exit.
	// So, it is safe to cleanup (in the "Start" function) once probeLoop returns.
	p.probeLoop(ctx, resultsChan)
	p.cleanup()
	return
}
