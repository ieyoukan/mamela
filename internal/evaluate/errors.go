package evaluate

import (
	"fmt"
	"strings"
)

// Code is a stable reason code for an evaluation failure.
type Code string

const (
	CodeToolchain          Code = "MAM-EVAL-0001"
	CodeBuildFailed        Code = "MAM-EVAL-0101"
	CodeBuildTimeout       Code = "MAM-EVAL-0102"
	CodeDependencyMismatch Code = "MAM-EVAL-0103"
	CodeReplaceMissing     Code = "MAM-EVAL-0104"
	CodeNotComponent       Code = "MAM-EVAL-0201"
	CodeImportDenied       Code = "MAM-EVAL-0202"
	CodeProgramError       Code = "MAM-EVAL-0301"
	CodeProgramDefect      Code = "MAM-EVAL-0302"
	CodeTimeout            Code = "MAM-EVAL-0303"
	CodeMemoryLimit        Code = "MAM-EVAL-0304"
	CodeOutputLimit        Code = "MAM-EVAL-0305"
	CodeAbnormalExit       Code = "MAM-EVAL-0306"
	CodeNotDeterministic   Code = "MAM-EVAL-0401"

	// The files a declaration takes texts from (contentFrom, files.go): each says the path and what
	// to do. They are reported as problems of the graph, at the member that names the file.
	CodeFileMissing    Code = "MAM-EVAL-0501" // not in the Pome (or it cannot be read)
	CodeFileOutside    Code = "MAM-EVAL-0502" // not a clean relative path inside the Pome
	CodeFileLink       Code = "MAM-EVAL-0503" // the file, or a directory on its way, is a symbolic link
	CodeFileNotRegular Code = "MAM-EVAL-0504" // a directory, a named pipe, a device
	CodeFileNotText    Code = "MAM-EVAL-0505" // not UTF-8, or it has a NUL
	CodeFileTooLarge   Code = "MAM-EVAL-0506" // larger than the member takes, or the files together larger than MaxFileTexts

	// mamela.sum, the sha256 of what a declaration downloads (sums.go).
	CodeSumMissing   Code = "MAM-EVAL-0601" // the declaration leaves a sha256 to mamela.sum, which does not have it
	CodeSumMalformed Code = "MAM-EVAL-0602" // mamela.sum is not the file mamela pin writes
)

// Stage is the step of evaluation that failed.
type Stage string

const (
	StageToolchain Stage = "toolchain"
	StageBuild     Stage = "build"
	StageInspect   Stage = "inspect"
	StageRun       Stage = "run"
	StageCompare   Stage = "compare"
	StageSums      Stage = "sums" // mamela.sum
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
