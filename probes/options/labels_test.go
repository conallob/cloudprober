// Copyright 2017-2021 The Cloudprober Authors.
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

package options

import (
	"net"
	"reflect"
	"testing"

	configpb "github.com/cloudprober/cloudprober/probes/proto"
	"github.com/cloudprober/cloudprober/targets/endpoint"
	"google.golang.org/protobuf/proto"
)

var configWithAdditionalLabels = &configpb.ProbeDef{
	AdditionalLabel: []*configpb.AdditionalLabel{
		{
			Key:   proto.String("src_zone"),
			Value: proto.String("zoneA"),
		},
		{
			Key:   proto.String("dst_zone"),
			Value: proto.String("@target.label.zone@"),
		},
		{
			Key:   proto.String("dst_zone_label"),
			Value: proto.String("zone:@target.label.zone@"),
		},
		{
			Key:   proto.String("dst_name"),
			Value: proto.String("@target.name@"),
		},
		{
			Key:   proto.String("dst"),
			Value: proto.String("@target.label.zone@:@target.name@:@target.port@"),
		},
		{
			Key:   proto.String("dst_ip_port"),
			Value: proto.String("@target.ip@:@target.port@"),
		},
		{
			Key:   proto.String("bad_label"),
			Value: proto.String("@target.metadata@:@unknown@"),
		},
		{
			Key:   proto.String("incomplete_label"),
			Value: proto.String("@target.label.zone@:@target.name"),
		},
	},
}

func TestParseAdditionalLabel(t *testing.T) {
	expected := []*AdditionalLabel{
		{
			Key:         "src_zone",
			staticValue: "zoneA",
			valueParts:  []string{"zoneA"},
		},
		{
			Key:        "dst_zone",
			valueParts: []string{"", "target.label.zone", ""},
			tokens:     []targetToken{{tokenType: label, labelKey: "zone"}},
		},
		{
			Key:        "dst_zone_label",
			valueParts: []string{"zone:", "target.label.zone", ""},
			tokens:     []targetToken{{tokenType: label, labelKey: "zone"}},
		},
		{
			Key:        "dst_name",
			valueParts: []string{"", "target.name", ""},
			tokens:     []targetToken{{tokenType: name}},
		},
		{
			Key:        "dst",
			valueParts: []string{"", "target.label.zone", ":", "target.name", ":", "target.port", ""},
			tokens:     []targetToken{{tokenType: label, labelKey: "zone"}, {tokenType: name}, {tokenType: port}},
		},
		{
			Key:        "dst_ip_port",
			valueParts: []string{"", "target.ip", ":", "target.port", ""},
			tokens:     []targetToken{{tokenType: ip}, {tokenType: port}}},
		{
			Key:         "bad_label",
			staticValue: "@target.metadata@:@unknown@",
			valueParts:  []string{"", "target.metadata", ":", "unknown", ""},
		},
		{
			Key:        "incomplete_label",
			valueParts: []string{"", "target.label.zone", ":", "@target.name"},
			tokens:     []targetToken{{tokenType: label, labelKey: "zone"}},
		},
	}

	for i, alpb := range configWithAdditionalLabels.GetAdditionalLabel() {
		t.Run(alpb.GetKey(), func(t *testing.T) {
			al := ParseAdditionalLabel(alpb)
			if !reflect.DeepEqual(al, expected[i]) {
				t.Errorf("Additional labels not parsed correctly. Got=\n%#v\nWanted=\n%#v", al, expected[i])
			}
		})
	}
}

func TestUpdateAdditionalLabel(t *testing.T) {
	aLabels := parseAdditionalLabels(configWithAdditionalLabels)

	endpoints := map[string]endpoint.Endpoint{
		"target1": {Name: "target1", Labels: map[string]string{}, IP: net.ParseIP("1.2.3.4"), Port: 80},
		"target2": {Name: "target2", Labels: map[string]string{"zone": "zoneB"}, Port: 8080},
		"target3": {Name: "target3", Port: 8080},
	}

	// Verify that we got the correct additional lables and also update them while
	// iterating over them.
	for _, al := range aLabels {
		al.UpdateForTarget(endpoints["target1"], "", 0)
		al.UpdateForTarget(endpoints["target2"], "", 0)
		al.UpdateForTarget(endpoints["target3"], "2.3.4.5", 9000)
	}

	expectedLabels := map[string][][2]string{
		"target1": {
			{"src_zone", "zoneA"},
			{"dst_zone", ""},
			{"dst_zone_label", "zone:"},
			{"dst_name", "target1"},
			{"dst", ":target1:80"},
			{"dst_ip_port", "1.2.3.4:80"},
			{"bad_label", "@target.metadata@:@unknown@"},
			{"incomplete_label", ":@target.name"},
		},
		"target2": {
			{"src_zone", "zoneA"},
			{"dst_zone", "zoneB"},
			{"dst_zone_label", "zone:zoneB"},
			{"dst_name", "target2"},
			{"dst", "zoneB:target2:8080"},
			{"dst_ip_port", ":8080"},
			{"bad_label", "@target.metadata@:@unknown@"},
			{"incomplete_label", "zoneB:@target.name"},
		},
		"target3": {
			{"src_zone", "zoneA"},
			{"dst_zone", ""},
			{"dst_zone_label", "zone:"},
			{"dst_name", "target3"},
			{"dst", ":target3:9000"},
			{"dst_ip_port", "2.3.4.5:9000"},
			{"bad_label", "@target.metadata@:@unknown@"},
			{"incomplete_label", ":@target.name"},
		},
	}

	for target, labels := range expectedLabels {
		var gotLabels [][2]string

		for _, al := range aLabels {
			k, v := al.KeyValueForTarget(endpoints[target])
			gotLabels = append(gotLabels, [2]string{k, v})
		}

		if !reflect.DeepEqual(gotLabels, labels) {
			t.Errorf("Didn't get expected labels for the target: %s. Got=%v, Expected=%v", target, gotLabels, labels)
		}
	}
}
