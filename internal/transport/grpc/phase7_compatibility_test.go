package grpc

import (
	"os"

	"testing"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

func phase6Descriptors(t *testing.T) *protoregistry.Files {
	t.Helper()
	data, err := os.ReadFile("testdata/phase6-agent.desc")
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

// The fixture contains the entire pre-Phase7 contract, rather than a selected
// list of familiar numbers. Every historical field and enum keeps its meaning.
func TestPhase7PreservesEntirePhase6Descriptor(t *testing.T) {
	files := phase6Descriptors(t)
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
