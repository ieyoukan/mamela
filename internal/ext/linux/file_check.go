package linux

import (
	"fmt"
	"strings"
)

// A file's check (NodeFile's check) is a validator compiled into mamelet: one fixed program with
// fixed arguments that reads a service's configuration and says whether the service would accept it.
// A declaration only names one of this list; mamelet runs the program the name maps to (an absolute
// path, no shell, nothing of the declaration in its arguments), so the rule that mamelet runs fixed
// programs only (ADR 0016 §2) holds. A validator is added only with a test of what mamelet does with
// it (file_check_mamelet_test.go).
//
// mamelet runs the check of a group once: after it has written every file of the round's grants that
// names it (two files of nginx may be valid only together), before anything reloads or restarts a
// service. If the check fails, it puts every file of the group back as it was (and removes those that
// did not exist), reloads and restarts nothing, and reports what the program said (file_check_mamelet.go).

// validator is a check mamelet runs.
type validator struct {
	Name    string   // as a declaration names it: "nginx"
	Program string   // absolute path
	Args    []string // fixed
	Shown   string   // for a person, in the plan: "nginx -t"
}

// validators are the checks mamelet runs, by name (sorted).
var validators = []validator{
	{Name: "nginx", Program: "/usr/sbin/nginx", Args: []string{"-t", "-q"}, Shown: "nginx -t"},
}

// validatorOf is the check called name (ok false: mamelet has none of that name).
func validatorOf(name string) (validator, bool) {
	for _, v := range validators {
		if v.Name == name {
			return v, true
		}
	}
	return validator{}, false
}

// validatorPrograms are the programs of the checks (mamelet runs them: mamelet.Extension.Programs).
func validatorPrograms() []string {
	var out []string
	for _, v := range validators {
		out = append(out, v.Program)
	}
	return out
}

// checkProblem says what is wrong with the check a file names ("" nothing: none, or one mamelet has).
func checkProblem(check string) string {
	if _, ok := validatorOf(check); check == "" || ok {
		return ""
	}
	return fmt.Sprintf("%q is not a check mamelet runs", check)
}

// checkHint lists the checks a declaration may name.
func checkHint() string {
	var known []string
	for _, v := range validators {
		known = append(known, fmt.Sprintf("%q (%s)", v.Name, v.Shown))
	}
	return "name one of the checks compiled into mamelet: " + strings.Join(known, ", ") +
		"; leave it out for a file no program checks (mamelet runs no program a declaration names)"
}

// checkText is the plan's line of a file's check: what runs, when, and what happens if it fails.
func checkText(check string) string {
	v, _ := validatorOf(check)
	return v.Shown + " (once the files of this node with this check are written; if it fails, they are put back as they were, and nothing is reloaded or restarted)"
}

// reloadText is the plan's line of the services a file reloads.
func reloadText(reload []string) string {
	return strings.Join(reload, ", ") + " (each only if it is active: systemctl reload)"
}

// unitOf is a service's name without ".service".
func unitOf(svc string) string { return strings.TrimSuffix(svc, ".service") }

// overlapProblem says which service a file names both to restart and to reload ("" none): it is one
// or the other.
func overlapProblem(restart, reload []string) string {
	for _, a := range reload {
		for _, b := range restart {
			if unitOf(a) == unitOf(b) {
				return fmt.Sprintf("service %s is named both in restart and in reload", a)
			}
		}
	}
	return ""
}
