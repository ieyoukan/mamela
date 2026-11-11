package catalogs

import (
	"errors"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// IAM is held to what ECS needs: roles under /mamela/, assumed only by ECS tasks or EC2, with only
// the two AWS-managed ECS policies attached. Any other value is refused before it is sent.
func TestIAMIsHeldToECS(t *testing.T) {
	entry := func(action string) protocol.Entry {
		for _, e := range IAM.Entries {
			if e.Fixed["Action"] == action {
				return e
			}
		}
		t.Fatalf("no %s", action)
		return protocol.Entry{}
	}
	attach := entry("AttachRolePolicy")
	if err := attach.CheckLimits(map[string][]string{"RoleName": {"r"}, "PolicyArn": {PolicyECSTaskExecution}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := attach.CheckLimits(map[string][]string{"PolicyArn": {"arn:aws:iam::aws:policy/AdministratorAccess"}}, nil); !errors.Is(err, protocol.ErrValueNotAllowed) {
		t.Fatalf("an administrator policy: %v", err)
	}
	create := entry("CreateRole")
	for name, params := range map[string]map[string][]string{
		"another path":  {"Path": {"/"}, "AssumeRolePolicyDocument": {TrustEC2}},
		"another trust": {"Path": {IAMPath}, "AssumeRolePolicyDocument": {strings.Replace(TrustEC2, "ec2.amazonaws.com", "lambda.amazonaws.com", 1)}},
	} {
		if err := create.CheckLimits(params, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := create.CheckLimits(map[string][]string{"Path": {IAMPath}, "AssumeRolePolicyDocument": {TrustECSTasks}}, nil); err != nil {
		t.Fatal(err)
	}
}

// A task definition's size reads from ECS CPU units and MiB.
func TestTaskDefinitionMeasures(t *testing.T) {
	var reg protocol.Entry
	for _, e := range ECS.Entries {
		if strings.HasSuffix(e.Fixed["X-Amz-Target"], ".RegisterTaskDefinition") {
			reg = e
		}
	}
	limits := map[string]int64{protocol.MeasureCPU: 1, protocol.MeasureMemoryMiB: 1024}
	if err := reg.CheckLimits(map[string][]string{"cpu": {`"256"`}, "memory": {`"512"`}}, limits); err != nil {
		t.Fatal(err)
	}
	if err := reg.CheckLimits(map[string][]string{"cpu": {`"2048"`}, "memory": {`"512"`}}, limits); err == nil {
		t.Fatal("2 vCPUs over a limit of 1")
	}
}
