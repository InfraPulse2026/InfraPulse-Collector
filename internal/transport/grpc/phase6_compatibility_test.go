package grpc

import (
	"os"
	"reflect"
	"testing"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func phase5Descriptors(t *testing.T) *protoregistry.Files {
	t.Helper()
	data, err := os.ReadFile("testdata/phase5-agent.desc")
	if err != nil {
		t.Fatal(err)
	}
	set := new(descriptorpb.FileDescriptorSet)
	if err = proto.Unmarshal(data, set); err != nil {
		t.Fatal(err)
	}
	files, err := protodesc.NewFiles(set)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// The fixture contains the entire pre-Phase6 contract, rather than a selected
// list of familiar numbers. Every historical field and enum keeps its meaning.
func TestPhase6PreservesEntirePhase5Descriptor(t *testing.T) {
	files := phase5Descriptors(t)
	previous, err := files.FindFileByPath("infrapulse/v1/agent.proto")
	if err != nil {
		t.Fatal(err)
	}
	current := pb.File_infrapulse_v1_agent_proto
	var checkMessages func(protoreflect.MessageDescriptors)
	checkMessages = func(messages protoreflect.MessageDescriptors) {
		for i := 0; i < messages.Len(); i++ {
			old := messages.Get(i)
			found := current.Messages().ByName(old.Name())
			if found == nil {
				t.Fatalf("legacy message removed: %s", old.FullName())
			}
			for j := 0; j < old.Fields().Len(); j++ {
				field := old.Fields().Get(j)
				newField := found.Fields().ByNumber(field.Number())
				if newField == nil || !proto.Equal(protodesc.ToFieldDescriptorProto(field), protodesc.ToFieldDescriptorProto(newField)) {
					t.Errorf("legacy field changed: %s", field.FullName())
				}
			}
			for j := 0; j < found.Fields().Len(); j++ {
				field := found.Fields().Get(j)
				if old.ReservedRanges().Has(field.Number()) || old.ReservedNames().Has(field.Name()) {
					t.Errorf("reserved legacy field reused: %s", field.FullName())
				}
			}
			checkMessages(old.Messages())
		}
	}
	checkMessages(previous.Messages())
	for i := 0; i < previous.Enums().Len(); i++ {
		old := previous.Enums().Get(i)
		found := current.Enums().ByName(old.Name())
		if found == nil {
			t.Fatalf("legacy enum removed: %s", old.Name())
		}
		for j := 0; j < old.Values().Len(); j++ {
			value := old.Values().Get(j)
			now := found.Values().ByNumber(value.Number())
			if now == nil || now.Name() != value.Name() {
				t.Errorf("legacy enum changed: %s", value.FullName())
			}
		}
	}
	for i := 0; i < previous.Services().Len(); i++ {
		old := previous.Services().Get(i)
		now := current.Services().ByName(old.Name())
		if now == nil || !proto.Equal(protodesc.ToServiceDescriptorProto(old), protodesc.ToServiceDescriptorProto(now)) {
			t.Errorf("legacy RPC changed: %s", old.FullName())
		}
	}
}

func TestPhase5PeerAcceptsNewTopologyAndPreservesUnknowns(t *testing.T) {
	u := remoteTopology()
	o := evidenceObservation("lldp", "snmp")
	u.Observation = o
	u.Observations[0].Observation = o
	u.Observations[0].Kind = "observed_l2_neighbor"
	e := testEnvelope()
	e.Topology = &u
	p, err := Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := proto.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	files := phase5Descriptors(t)
	desc, err := files.FindDescriptorByName("infrapulse.v1.AgentEnvelope")
	if err != nil {
		t.Fatal(err)
	}
	old := dynamicpb.NewMessage(desc.(protoreflect.MessageDescriptor))
	if err = proto.Unmarshal(bytes, old); err != nil {
		t.Fatal("old peer rejected additive fields", err)
	}
	topology := old.Get(old.Descriptor().Fields().ByNumber(14)).Message()
	neighbor := topology.Get(topology.Descriptor().Fields().ByNumber(8)).List().Get(0).Message()
	if neighbor.Get(neighbor.Descriptor().Fields().ByNumber(8)).Uint() != 4 || neighbor.Get(neighbor.Descriptor().Fields().ByNumber(10)).Uint() != 3 {
		t.Fatal("old peer interpreted legacy subtypes differently")
	}
	if len(topology.GetUnknown()) == 0 || len(neighbor.GetUnknown()) == 0 {
		t.Fatal("old peer did not preserve additive fields")
	}
	forwarded, err := proto.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	now := new(pb.AgentEnvelope)
	if err = proto.Unmarshal(forwarded, now); err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeEnvelope(now)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, e) {
		t.Fatal("old peer forwarding lost observations")
	}
}
