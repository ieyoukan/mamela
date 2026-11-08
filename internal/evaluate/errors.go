package evaluate

import (
	"fmt"
	"strings"
)

// Code is a stable reason code for an evaluation failure.
type Code string

const (
	CodeToolchain        Code = "MAM-EVAL-0001"
	CodeBuildFailed      Code = "MAM-EVAL-0101"
	CodeBuildTimeout     Code = "MAM-EVAL-0102"
	CodeNotComponent     Code = "MAM-EVAL-0201"
	CodeImportDenied     Code = "MAM-EVAL-0202"
	CodeProgramError     Code = "MAM-EVAL-0301"
	CodeProgramDefect    Code = "MAM-EVAL-0302"
	CodeTimeout          Code = "MAM-EVAL-0303"
	CodeMemoryLimit      Code = "MAM-EVAL-0304"
	CodeOutputLimit      Code = "MAM-EVAL-0305"
	CodeAbnormalExit     Code = "MAM-EVAL-0306"
	CodeNotDeterministic Code = "MAM-EVAL-0401"
)

// Stage is the step of evaluation that failed.
type Stage string

const (
	StageToolchain Stage = "toolchain"
	StageBuild     Stage = "build"
	StageInspect   Stage = "inspect"
	StageRun       Stage = "run"
	StageCompare   Stage = "compare"
)

// Error is an evaluation failure written for a person.
type Error struct {
	Code    Code
	Stage   Stage
	Message string // what happened and why
	Hint    string // what to do
	Detail  string // diagnostics from the compiler or the program (stderr), trimmed
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", e.Code, e.Message)
	if e.Hint != "" {
		fmt.Fprintf(&b, "\n  hint: %s", e.Hint)
	}
	if d := strings.TrimSpace(e.Detail); d != "" {
		b.WriteString("\n  detail:\n")
		for _, line := range strings.Split(d, "\n") {
			b.WriteString("    " + line + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
