package mamelet

// Release is mamelet's release. A mamelet built by scripts/build-mamelet.sh reports it with the
// commit it was built from ("0.10.0+849289e") as its version, and the plan names that version when a
// node's mamelet is too old for what is declared on it (MAM-PLAN-0122).
//
// 0.10.0 brought the generic wire of node extensions (the revision's ext, the report's extensions,
// ext and extRefused: noderev's ext.go) and with it the linux extension (NodeFile). A Control Plane
// of that wire reads the reports of older mamelets, and gives them no ext; an older Control Plane
// cannot read the reports of a 0.10.0 mamelet. Update the Control Plane first, then the mamelets
// (docs/mamelet-update.md).
const Release = "0.10.0"
