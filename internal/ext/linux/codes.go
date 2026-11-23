package linux

import "github.com/ieyoukan/mamela/internal/graph"

// reservation is a range of codes of one kind of the extension (Kind "": the extension's, for every
// kind, such as MAM-PLAN-0219 of targets.go, and kept for the kinds that follow).
type reservation struct {
	Kind     graph.Kind
	Prefix   string // "MAM-PLAN", "MAM-GRAPH", "MAM-POL"
	From, To int
}

// reserved are the codes of the extension, by kind: the one record of them. A kind's plan notes,
// graph warnings and policy decisions are in its ranges; doc.go and docs/reason-codes.md say the same
// (codes_test.go checks both, and every code the extension registers, against this).
var reserved = []reservation{
	{KindFile, "MAM-PLAN", 140, 144}, {KindFile, "MAM-PLAN", 210, 211}, {KindFile, "MAM-PLAN", 218, 218}, {KindFile, "MAM-PLAN", 310, 310},
	{KindFile, "MAM-PLAN", 230, 232}, // its checks (file_check.go)
	{KindFile, "MAM-GRAPH", 203, 204}, {KindFile, "MAM-POL", 220, 220},

	{KindUser, "MAM-PLAN", 145, 149}, {KindUser, "MAM-PLAN", 212, 213}, {KindUser, "MAM-PLAN", 311, 311},
	{KindUser, "MAM-GRAPH", 206, 206}, {KindUser, "MAM-POL", 221, 222},

	{KindAptRepo, "MAM-PLAN", 150, 154}, {KindAptRepo, "MAM-PLAN", 214, 215}, {KindAptRepo, "MAM-PLAN", 312, 312},
	{KindAptRepo, "MAM-GRAPH", 207, 214}, {KindAptRepo, "MAM-POL", 223, 224},

	{KindArtifact, "MAM-PLAN", 155, 159}, {KindArtifact, "MAM-PLAN", 216, 217}, {KindArtifact, "MAM-PLAN", 313, 313},
	{KindArtifact, "MAM-GRAPH", 215, 219}, {KindArtifact, "MAM-POL", 225, 226},

	{KindAptUpgrades, "MAM-PLAN", 165, 169}, {KindAptUpgrades, "MAM-PLAN", 225, 226}, {KindAptUpgrades, "MAM-POL", 227, 227},

	{"", "MAM-PLAN", 219, 219}, {"", "MAM-PLAN", 314, 319}, {"", "MAM-GRAPH", 205, 205},
}
