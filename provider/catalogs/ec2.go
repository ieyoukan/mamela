package catalogs

import "github.com/ieyoukan/mamela/provider/protocol"

// EC2Version is the version of the EC2 query API the catalog describes.
const EC2Version = "2016-11-15"

// EC2 is the part of the Amazon EC2 query API that providers of EC2 instances use (ADR 0005).
// Every call is POST / with a form body; the fixed parameters Action and Version tell them apart.
// RunInstances also fixes MinCount and MaxCount to 1: one change makes one instance. Requests are
// signed with SigV4 (auth "aws-sigv4" in mamela.providers.toml).
//
// An instance's size is its instance type, which the measures read with a table of the types
// Mamela knows (vCPUs, MiB): a type not in it is refused, never guessed.
var EC2 = protocol.Catalog{API: "ec2", Entries: []protocol.Entry{
	ec2Call("DescribeInstances", protocol.EffectRead, nil, "Filter.1.Name", "Filter.1.Value.1", "Filter.2.Name", "Filter.2.Value.1",
		"Filter.2.Value.2", "Filter.2.Value.3", "Filter.2.Value.4", "MaxResults", "NextToken"),
	withMeasures(ec2Call("RunInstances", protocol.EffectCreate, map[string]string{"MinCount": "1", "MaxCount": "1"},
		"MinCount", "MaxCount", "ImageId", "InstanceType", "SubnetId", "SecurityGroupId.1", "UserData", "ClientToken",
		"BlockDeviceMapping.1.DeviceName", "BlockDeviceMapping.1.Ebs.VolumeSize", "BlockDeviceMapping.1.Ebs.VolumeType",
		"MetadataOptions.HttpTokens", "TagSpecification.1.ResourceType",
		"TagSpecification.1.Tag.1.Key", "TagSpecification.1.Tag.1.Value", "TagSpecification.1.Tag.2.Key", "TagSpecification.1.Tag.2.Value",
		"TagSpecification.1.Tag.3.Key", "TagSpecification.1.Tag.3.Value"),
		protocol.Measure{Param: "InstanceType", Name: protocol.MeasureCPU, Format: protocol.FormatTable, Table: ec2VCPUs()},
		protocol.Measure{Param: "InstanceType", Name: protocol.MeasureMemoryMiB, Format: protocol.FormatTable, Table: ec2MemoryMiB()},
		protocol.Measure{Param: "BlockDeviceMapping.1.Ebs.VolumeSize", Name: protocol.MeasureDiskGiB, Format: protocol.FormatCount}),
	ec2Call("StartInstances", protocol.EffectRestart, nil, "InstanceId.1"),
	ec2Call("StopInstances", protocol.EffectRestart, nil, "InstanceId.1"),
	ec2Call("RebootInstances", protocol.EffectRestart, nil, "InstanceId.1"),
	withMeasures(ec2Call("ModifyInstanceAttribute", protocol.EffectChange, nil, "InstanceId", "InstanceType.Value"),
		protocol.Measure{Param: "InstanceType.Value", Name: protocol.MeasureCPU, Format: protocol.FormatTable, Table: ec2VCPUs()},
		protocol.Measure{Param: "InstanceType.Value", Name: protocol.MeasureMemoryMiB, Format: protocol.FormatTable, Table: ec2MemoryMiB()}),
	ec2Call("CreateTags", protocol.EffectChange, nil, "ResourceId.1", "Tag.1.Key", "Tag.1.Value", "Tag.2.Key", "Tag.2.Value",
		"Tag.3.Key", "Tag.3.Value"),
	ec2Call("TerminateInstances", protocol.EffectLoseData, nil, "InstanceId.1"),
}}

func ec2Call(action string, effect protocol.Effect, fixed map[string]string, form ...string) protocol.Entry {
	f := map[string]string{"Action": action, "Version": EC2Version}
	for k, v := range fixed {
		f[k] = v
	}
	return protocol.Entry{Method: "POST", Path: "/", Effect: effect, Form: append([]string{"Action", "Version"}, form...), Fixed: f}
}

func bind(e protocol.Entry, params ...string) protocol.Entry {
	e.Bind = params
	return e
}

func withMeasures(e protocol.Entry, ms ...protocol.Measure) protocol.Entry {
	e.Measures = ms
	return e
}

// ec2Types are the instance types Mamela knows the size of: vCPUs and memory in MiB.
var ec2Types = map[string][2]int64{
	"t3.nano": {2, 512}, "t3.micro": {2, 1024}, "t3.small": {2, 2048}, "t3.medium": {2, 4096},
	"t3.large": {2, 8192}, "t3.xlarge": {4, 16384}, "t3.2xlarge": {8, 32768},
	"t3a.nano": {2, 512}, "t3a.micro": {2, 1024}, "t3a.small": {2, 2048}, "t3a.medium": {2, 4096},
	"t3a.large": {2, 8192}, "t3a.xlarge": {4, 16384}, "t3a.2xlarge": {8, 32768},
	"t4g.nano": {2, 512}, "t4g.micro": {2, 1024}, "t4g.small": {2, 2048}, "t4g.medium": {2, 4096},
	"t4g.large": {2, 8192}, "t4g.xlarge": {4, 16384}, "t4g.2xlarge": {8, 32768},
	"m6i.large": {2, 8192}, "m6i.xlarge": {4, 16384}, "m6i.2xlarge": {8, 32768},
	"m7i.large": {2, 8192}, "m7i.xlarge": {4, 16384}, "m7i.2xlarge": {8, 32768},
	"c6i.large": {2, 4096}, "c6i.xlarge": {4, 8192}, "c6i.2xlarge": {8, 16384},
	"r6i.large": {2, 16384}, "r6i.xlarge": {4, 32768},
}

func ec2VCPUs() map[string]int64 {
	out := map[string]int64{}
	for t, s := range ec2Types {
		out[t] = s[0]
	}
	return out
}

func ec2MemoryMiB() map[string]int64 {
	out := map[string]int64{}
	for t, s := range ec2Types {
		out[t] = s[1]
	}
	return out
}
