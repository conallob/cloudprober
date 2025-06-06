// Copyright 2024 The Cloudprober Authors.
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

package browser

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cloudprober/cloudprober/metrics"
	configpb "github.com/cloudprober/cloudprober/probes/browser/proto"
	"github.com/cloudprober/cloudprober/probes/options"
	"github.com/cloudprober/cloudprober/state"
	"github.com/cloudprober/cloudprober/targets/endpoint"
	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/proto"
)

func TestProbePrepareCommand(t *testing.T) {
	os.Setenv("PLAYWRIGHT_DIR", "/playwright")
	defer os.Unsetenv("PLAYWRIGHT_DIR")

	baseEnvVars := func(pwDir string) []string {
		return []string{"NODE_PATH=" + pwDir + "/node_modules", "PLAYWRIGHT_HTML_REPORT={OUTPUT_DIR}/" + playwrightReportDir, "PLAYWRIGHT_HTML_OPEN=never"}
	}

	cmdLine := func(npxPath string) []string {
		return []string{npxPath, "playwright", "test", "--config={WORKDIR}/playwright.config.ts", "--output=${OUTPUT_DIR}/results", "--reporter=html,{WORKDIR}/cloudprober-reporter.ts"}
	}

	baseWantEMLabels := [][2]string{{"ptype", "browser"}, {"probe", "test_browser"}, {"dst", ""}}

	testDir := "/tests"

	tests := []struct {
		name               string
		disableAggregation bool
		npxPath            string
		playwrightDir      string
		testSpec           []string
		target             endpoint.Endpoint
		wantCmdLine        []string
		wantEnvVars        []string
		wantWorkDir        string
		wantEMLabels       [][2]string
	}{
		{
			name:         "default",
			wantCmdLine:  cmdLine("npx"),
			wantEnvVars:  baseEnvVars("/playwright"),
			wantWorkDir:  "/playwright",
			wantEMLabels: baseWantEMLabels,
		},
		{
			name:         "with_target",
			target:       endpoint.Endpoint{Name: "test_target", IP: net.ParseIP("12.12.12.12"), Port: 9313, Labels: map[string]string{"env": "prod"}},
			wantCmdLine:  cmdLine("npx"),
			wantEnvVars:  append(baseEnvVars("/playwright"), "target_name=test_target", "target_ip=12.12.12.12", "target_port=9313", "target_label_env=prod"),
			wantWorkDir:  "/playwright",
			wantEMLabels: [][2]string{{"ptype", "browser"}, {"probe", "test_browser"}, {"dst", "test_target:9313"}},
		},
		{
			name:               "disable_aggregation",
			disableAggregation: true,
			wantCmdLine:        cmdLine("npx"),
			wantEnvVars:        baseEnvVars("/playwright"),
			wantWorkDir:        "/playwright",
			wantEMLabels:       append(baseWantEMLabels, [2]string{"run_id", "0"}),
		},
		{
			name:          "with_playwright_dir",
			playwrightDir: "/app",
			wantCmdLine:   cmdLine("npx"),
			wantEnvVars:   baseEnvVars("/app"),
			wantWorkDir:   "/app",
			wantEMLabels:  baseWantEMLabels,
		},
		{
			name:         "with_npx_path",
			npxPath:      "/usr/bin/npx",
			wantCmdLine:  cmdLine("/usr/bin/npx"),
			wantEnvVars:  baseEnvVars("/playwright"),
			wantWorkDir:  "/playwright",
			wantEMLabels: baseWantEMLabels,
		},
		{
			name:         "with_test_spec",
			testSpec:     []string{"test_spec_1", "test_spec_2"},
			wantCmdLine:  append(cmdLine("npx"), "^.*/test_spec_1$", "^.*/test_spec_2$"),
			wantEnvVars:  baseEnvVars("/playwright"),
			wantWorkDir:  "/playwright",
			wantEMLabels: baseWantEMLabels,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conf := &configpb.ProbeConf{
				TestSpec: tt.testSpec,
				TestDir:  &testDir,
				TestMetricsOptions: &configpb.TestMetricsOptions{
					DisableAggregation: &tt.disableAggregation,
				},
			}
			if tt.playwrightDir != "" {
				conf.PlaywrightDir = &tt.playwrightDir
			}
			if tt.npxPath != "" {
				conf.NpxPath = proto.String(filepath.FromSlash(tt.npxPath))
			}

			opts := options.DefaultOptions()
			opts.ProbeConf = conf
			p := &Probe{}
			if err := p.Init("test_browser", opts); err != nil {
				t.Fatalf("Error in probe initialization: %v", err)
			}

			ts := time.Now()
			cmd, _ := p.prepareCommand(tt.target, ts)

			outputDir := p.outputDirPath(tt.target, ts)
			for i, arg := range tt.wantCmdLine {
				tt.wantCmdLine[i] = strings.ReplaceAll(arg, "{WORKDIR}", p.workdir)
				tt.wantCmdLine[i] = filepath.FromSlash(strings.ReplaceAll(tt.wantCmdLine[i], "${OUTPUT_DIR}", outputDir))
				if runtime.GOOS == "windows" {
					// For test specs, backslashes get escaped again by regexp.QuoteMeta.
					tt.wantCmdLine[i] = strings.ReplaceAll(tt.wantCmdLine[i], `.*\`, `.*\\`)
				}
			}
			for i, envVar := range tt.wantEnvVars {
				tt.wantEnvVars[i] = filepath.FromSlash(strings.ReplaceAll(envVar, "{OUTPUT_DIR}", outputDir))
			}

			assert.Equal(t, tt.wantCmdLine, cmd.CmdLine)
			assert.Equal(t, tt.wantEnvVars, cmd.EnvVars)
			assert.Equal(t, tt.wantWorkDir, cmd.WorkDir)

			p.dataChan = make(chan *metrics.EventMetrics, 10)
			cmd.ProcessStreamingOutput([]byte("test_1_succeeded 1\n"))
			em := <-p.dataChan
			assert.Len(t, em.LabelsKeys(), len(tt.wantEMLabels))
			for _, label := range tt.wantEMLabels {
				assert.Equal(t, label[1], em.Label(label[0]), "label %s", label[0])
			}
		})
	}
}

func TestProbeOutputDirPath(t *testing.T) {
	tests := []struct {
		name      string
		outputDir string
		target    endpoint.Endpoint
		ts        time.Time
		want      string
	}{
		{
			name:      "default",
			outputDir: "/tmp/output",
			ts:        time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC),
			want:      "/tmp/output/2024-01-01/1704067200000",
		},
		{
			name:      "with_target",
			outputDir: "/tmp/output",
			target:    endpoint.Endpoint{Name: "test_target"},
			ts:        time.Date(2024, time.February, 2, 12, 30, 45, 0, time.UTC),
			want:      "/tmp/output/2024-02-02/1706877045000/test_target",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Probe{outputDir: tt.outputDir}
			assert.Equal(t, filepath.FromSlash(tt.want), p.outputDirPath(tt.target, tt.ts))
		})
	}
}

func TestProbeInitTemplates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping test on Windows, path issues - not worth it")
	}

	tmpDir := t.TempDir()

	oldConfigFilePath := state.ConfigFilePath()
	defer state.SetConfigFilePath(oldConfigFilePath)
	state.SetConfigFilePath("/cfg/cloudprober.cfg")

	defaultConfigContains := []string{
		"testDir: \"/cfg\"",
		"screenshot: \"only-on-failure\"",
		"trace: \"off\"",
	}
	reporterContainTestLevel := []string{
		"print(`test_status",
		"print(`test_latency",
	}
	reporterContainStepLevel := []string{
		"print(`test_step_status",
		"print(`test_step_latency",
	}

	tests := []struct {
		name                string
		conf                *configpb.ProbeConf
		configContains      []string
		reporterContains    []string
		reporterNotContains []string
	}{
		{
			name: "default",
			conf: &configpb.ProbeConf{
				Workdir: proto.String(tmpDir),
			},
			configContains:      defaultConfigContains,
			reporterContains:    reporterContainTestLevel,
			reporterNotContains: reporterContainStepLevel,
		},
		{
			name: "with_config_dir",
			conf: &configpb.ProbeConf{
				TestDir: proto.String("/cfg/tests"),
				Workdir: proto.String(tmpDir),
			},
			configContains: []string{
				"testDir: \"/cfg/tests\"",
				"screenshot: \"only-on-failure\"",
				"trace: \"off\"",
			},
			reporterContains:    reporterContainTestLevel,
			reporterNotContains: reporterContainStepLevel,
		},
		{
			name: "with_screenshots_and_traces",
			conf: &configpb.ProbeConf{
				Workdir:                   proto.String(tmpDir),
				SaveScreenshotsForSuccess: proto.Bool(true),
				SaveTraces:                proto.Bool(true),
			},
			configContains: []string{
				"screenshot: \"on\"",
				"trace: \"on\"",
			},
			reporterContains:    reporterContainTestLevel,
			reporterNotContains: reporterContainStepLevel,
		},
		{
			name: "with_step_metrics",
			conf: &configpb.ProbeConf{
				Workdir: proto.String(tmpDir),
				TestMetricsOptions: &configpb.TestMetricsOptions{
					EnableStepMetrics: proto.Bool(true),
				},
			},
			configContains:   defaultConfigContains,
			reporterContains: append(reporterContainTestLevel, reporterContainStepLevel...),
		},
		{
			name: "disable_test_metrics",
			conf: &configpb.ProbeConf{
				Workdir: proto.String(tmpDir),
				TestMetricsOptions: &configpb.TestMetricsOptions{
					DisableTestMetrics: proto.Bool(true),
				},
			},
			configContains:      defaultConfigContains,
			reporterNotContains: append(reporterContainTestLevel, reporterContainStepLevel...),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Probe{
				name:    "test_probe",
				c:       tt.conf,
				opts:    options.DefaultOptions(),
				workdir: tmpDir,
			}

			err := p.initTemplates()
			if err != nil {
				t.Fatalf("initTemplates() error = %v", err)
			}

			// Verify playwright config file
			got, err := os.ReadFile(p.playwrightConfigPath)
			if err != nil {
				t.Fatalf("Error reading playwright config: %v", err)
			}
			for _, want := range tt.configContains {
				assert.Contains(t, string(got), want, "playwright config should contain: %s", want)
			}

			// Verify reporter file
			got, err = os.ReadFile(p.reporterPath)
			if err != nil {
				t.Fatalf("Error reading playwright config: %v", err)
			}
			for _, want := range tt.reporterContains {
				assert.Contains(t, string(got), want, "reporter file should contain: %s", want)
			}
			for _, want := range tt.reporterNotContains {
				assert.NotContains(t, string(got), want, "reporter file should not contain: %s", want)
			}
		})
	}
}

func TestPlaywrightGlobalTimeoutMsec(t *testing.T) {
	tests := []struct {
		name                 string
		timeout              time.Duration
		requestsPerProbe     int
		requestsIntervalMsec int
		want                 int64
	}{
		{
			name:    "single_request",
			timeout: 10 * time.Second,
			want:    9000,
		},
		{
			name:                 "multiple_requests",
			timeout:              20 * time.Second,
			requestsPerProbe:     3,
			requestsIntervalMsec: 1000,
			want:                 16200, // (20s - (3-1)*1s) - 0.9s (buffer)
		},
		{
			name:    "large_buffer",
			timeout: 120 * time.Second,
			want:    118000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Probe{
				opts: &options.Options{
					Timeout: tt.timeout,
				},
				c: &configpb.ProbeConf{
					RequestsPerProbe:     proto.Int32(int32(tt.requestsPerProbe)),
					RequestsIntervalMsec: proto.Int32(int32(tt.requestsIntervalMsec)),
				},
			}
			if got := p.playwrightGlobalTimeoutMsec(); got != tt.want {
				t.Errorf("playwrightGlobalTimeoutMsec() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProbeComputeTestSpecArgs(t *testing.T) {
	tests := []struct {
		name          string
		testDir       string
		testSpec      []string
		filterInclude string
		filterExclude string
		wantArgs      []string
		wantArgsWin   []string
	}{
		{
			name:     "no_spec_no_filter",
			testDir:  "/tests",
			testSpec: nil,
			wantArgs: []string{},
		},
		{
			name:        "single_spec_relative",
			testDir:     "/tests",
			testSpec:    []string{"myspec.js"},
			wantArgs:    []string{`^.*/myspec\.js$`},
			wantArgsWin: []string{`^.*\\myspec\.js$`},
		},
		{
			name:        "single_spec_absolute",
			testDir:     "/tests",
			testSpec:    []string{"/abs/path/spec.js"},
			wantArgs:    []string{`^/abs/path/spec\.js$`},
			wantArgsWin: []string{`^\\abs\\path\\spec\.js$`},
		},
		{
			name:     "multiple_specs_mixed",
			testDir:  "/dir",
			testSpec: []string{"foo.js", "/bar/baz.js"},
			wantArgs: []string{
				`^.*/foo\.js$`,
				`^/bar/baz\.js$`,
			},
			wantArgsWin: []string{
				`^.*\\foo\.js$`,
				`^\\bar\\baz\.js$`,
			},
		},
		{
			name:     "regex_spec",
			testDir:  "/dir",
			testSpec: []string{`^foo.*\.js$`},
			wantArgs: []string{`^foo.*\.js$`},
		},
		{
			name:          "with_include_filter",
			testDir:       "/dir",
			testSpec:      []string{"foo.js"},
			filterInclude: "mytest",
			wantArgs: []string{
				"--grep=mytest",
				`^.*/foo\.js$`,
			},
			wantArgsWin: []string{
				"--grep=mytest",
				`^.*\\foo\.js$`,
			},
		},
		{
			name:          "with_exclude_filter",
			testDir:       "/dir",
			testSpec:      []string{"foo.js"},
			filterExclude: "skipme",
			wantArgs: []string{
				"--grep-invert=skipme",
				`^.*/foo\.js$`,
			},
			wantArgsWin: []string{
				"--grep-invert=skipme",
				`^.*\\foo\.js$`,
			},
		},
		{
			name:          "with_both_filters",
			testDir:       "/dir",
			testSpec:      []string{"foo.js"},
			filterInclude: "mytest",
			filterExclude: "skipme",
			wantArgs: []string{
				"--grep=mytest",
				"--grep-invert=skipme",
				`^.*/foo\.js$`,
			},
			wantArgsWin: []string{
				"--grep=mytest",
				"--grep-invert=skipme",
				`^.*\\foo\.js$`,
			},
		},
		{
			name:     "multiple_specs_with_regex",
			testDir:  "/dir",
			testSpec: []string{"foo.js", `^bar.*\.js$`},
			wantArgs: []string{
				`^.*/foo\.js$`,
				`^bar.*\.js$`,
			},
			wantArgsWin: []string{
				`^.*\\foo\.js$`,
				`^bar.*\.js$`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conf := &configpb.ProbeConf{}
			for _, spec := range tt.testSpec {
				conf.TestSpec = append(conf.TestSpec, filepath.FromSlash(spec))
			}
			if tt.filterInclude != "" || tt.filterExclude != "" {
				conf.TestSpecFilter = &configpb.TestSpecFilter{}
				if tt.filterInclude != "" {
					conf.TestSpecFilter.Include = &tt.filterInclude
				}
				if tt.filterExclude != "" {
					conf.TestSpecFilter.Exclude = &tt.filterExclude
				}
			}
			p := &Probe{
				c:       conf,
				testDir: tt.testDir,
			}
			got := p.computeTestSpecArgs()
			if runtime.GOOS == "windows" {
				if tt.wantArgsWin == nil {
					tt.wantArgsWin = tt.wantArgs
				}
				assert.Equal(t, tt.wantArgsWin, got)
			} else {
				assert.Equal(t, tt.wantArgs, got)
			}
		})
	}
}
