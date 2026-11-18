// Package linux is the linux node extension (ADR 0016): a few safe primitives for what is inside a
// machine, which mamelet carries out with fixed code and fixed programs only (no shell, no command
// a declaration names). Recipes (an application's installation) are Emela functions in Pomes that
// compose them; a Go extension is for what needs orchestration across nodes (k3s).
//
// It is the first extension of the generic wire (stage 5 of docs/design/node-extensions.md): its
// part of a node's revision is Revision.Ext["linux"] (Part), its part of the report
// Report.Ext["linux"] (Observed), and a node gets the part only when its mamelet takes it
// (Report.Extensions, Version): a mamelet older than 0.10.0 gets nothing of it, and the plan blocks
// what is declared there (MAM-PLAN-0122).
//
// # Layout
//
// One file per kind and layer, shared helpers in files of their own:
//
//	register.go        the one registration with every layer (graph, plan, policy, node provider,
//	                   mamelet, noderev), from kinds: a kind adds itself to that list
//	wire.go            Part and Observed, their encoding, the grants in the part (noderev), what a
//	                   grant was planned against (Before: mamelet compares and swaps)
//	targets.go         what the part asks mamelet to observe (a union of the declarations planned,
//	                   bounded), and what the declaration in force has (InForce, annotate)
//	paths.go           CheckPath: the paths no kind writes (graph and mamelet check the same)
//	accounts.go        user and group names, and the node's /etc/passwd and /etc/group
//	fs.go              mamelet's view of the file system: no symbolic link is followed
//	nofollow_*.go      the opening of a file without following a link (x/sys/unix on Linux and macOS)
//	pending.go         what follows a write (daemon-reload, restarts), and what did not finish
//	graph.go           what concerns more than one resource: paths claimed twice, warnings
//	file.go            NodeFile: spec, decoding, validation (graph)
//	file_plan.go       NodeFile: plan, the start gate of the services it restarts, files kept
//	file_node.go       NodeFile: the node's revision (publish, prune, protect, grant)
//	file_mamelet.go    NodeFile: what mamelet observes and does
//	authkeys.go        the lines of authorized_keys (graph and mamelet read them alike)
//	user.go            NodeUser: spec, decoding, validation, warnings (graph)
//	user_plan.go       NodeUser: plan, the users files wait for, users kept
//	user_wire.go       NodeUser: its members of Part and Observed
//	user_node.go       NodeUser: the node's revision (publish, prune, protect, grant)
//	user_mamelet.go    NodeUser: what mamelet observes and does (useradd, usermod, authorized_keys)
//	artifact*.go       NodeArtifact, in the same layers (artifact_wire.go: its members of Part and
//	                   Observed); artifact_mamelet.go keeps mamelet's record of what it installed
//	download.go        downloads over https pinned by sha256, and a member of a .tar.gz: no type of
//	                   the extension, so that another (k3s) can use them
//	aptrepo.go         NodeAptRepo: spec, decoding, validation, the source's file (AptSources)
//	aptrepo_wire.go    NodeAptRepo: its members of Part and Observed, their grants and checks
//	aptrepo_plan.go    NodeAptRepo: plan, the gate of the packages of its node, sources kept
//	aptrepo_node.go    NodeAptRepo: the node's revision (publish, prune, protect, grant)
//	aptrepo_mamelet.go NodeAptRepo: what mamelet observes (its files, apt's lists, other sources)
//	                   and does (the key's download with download.go, the files, apt-get update)
//
// # Reserved for the kinds that follow
//
// The canonical order of a kind is part of every graph's digest and never changes. The core's kinds
// are 1, 2 and 5, k3s's 3 and 4, and the providers' kinds sort at 5 by name; the linux kinds come
// after all of them, in the order an apply should run them in (the operations of one plan run in
// canonical order): accounts, sources, downloads, then files.
//
//	kind          order  operations (linux.<verb>-<object>)  resource name
//	NodeUser      6      linux.create-user, linux.change-user  <node>/user/<name>
//	NodeAptRepo   7      linux.add-apt-repo                    <node>/apt-repo/<name>
//	NodeArtifact  8      linux.install-artifact                <node>/artifact/<path>
//	NodeFile      9      linux.write-file                      <node>/file/<path>
//
// The codes of each kind (its plan notes, graph warnings and policy decisions), and those kept for the
// kinds that follow, are in one table: reserved, in codes.go. docs/reason-codes.md reserves the same
// (codes_test.go checks it, and that every code the extension registers is in its kind's ranges).
//
// NodeFile and NodeArtifact share two notes (pending.go, file_plan.go): MAM-PLAN-0144, the warning of
// a file written below a directory a user other than root can change, and MAM-PLAN-0218, a file that
// is as declared but whose daemon-reload or restart did not finish after it was written. MAM-PLAN-0219
// is left (codes.go reserves 0144 and 0218 for both).
//
// # What mamelet opens
//
// fs.go and nofollow_*.go: mamelet runs as root, and the paths it observes may be in directories
// users can change (a home). It walks each directory by itself from the root (each one checked not
// to be a link, and to be the one it looked at), opens a file only with O_NOFOLLOW, O_NOCTTY and
// O_NONBLOCK, and only a regular file (fstat of the descriptor), so that no link a user swaps in can
// lead it to a terminal or a device. A directory it makes gets its owner and mode through a
// descriptor, never its name. A home is never taken from another user (user_mamelet.go).
package linux
