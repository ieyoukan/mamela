// Package k3s is the k3s node extension: the kinds NodeK3s and NodeK3sManifest, how they are
// planned and decided, what the Control Plane puts in a node's revision for them, and what mamelet
// does with that (docs/design/node-extensions.md). It registers itself with the core in init;
// cmd/mamela and cmd/mamelet compile it in with a blank import.
//
// The core does not depend on it: the core's packages know no k3s but through what an extension
// registers, and a test (TestCoreDoesNotDependOnExtensions) keeps it so. The wire types (the
// revision's K3s and Manifests, the report's K3sState, the snapshot's K3s) are still the core's
// noderev and state types: k3s has not moved to the generic part of the wire (Revision.Ext, which
// the linux extension uses) yet, so that its revisions, reports and digests stay as they were.
package k3s
