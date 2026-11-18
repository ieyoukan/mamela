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
//	wire.go            Part and Observed, their encoding, the grants in the part (noderev)
//	paths.go           CheckPath: the paths no kind writes (graph and mamelet check the same)
//	accounts.go        user and group names, and the node's /etc/passwd and /etc/group
//	fs.go              mamelet's view of the file system: no symbolic link is followed
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
//
// # Reserved for the kinds that follow
//
// The canonical order of a kind is part of every graph's digest and never changes. The core's kinds
// are 1, 2 and 5, k3s's 3 and 4, and the providers' kinds sort at 5 by name; the linux kinds come
// after all of them, in the order an apply should run them in (the operations of one plan run in
// canonical order): accounts, sources, downloads, then files.
//
//	kind          order  operations (linux.<verb>-<object>)  plan notes           graph warnings   policy
//	NodeUser      6      linux.create-user, linux.change-user MAM-PLAN-0145..0149  MAM-GRAPH-0206..  MAM-POL-0221..0222
//	NodeAptRepo   7      linux.add-apt-repo, ...              MAM-PLAN-0150..0154  ..0219           MAM-POL-0223..0224
//	NodeArtifact  8      linux.install-artifact, ...          MAM-PLAN-0155..0159  MAM-GRAPH-0215..  MAM-POL-0225..0226
//	NodeFile      9      linux.write-file                     MAM-PLAN-0140..0144  MAM-GRAPH-0203..  MAM-POL-0220
//
// The plan's observation problems of linux are MAM-PLAN-0210..0219 (NodeFile 0210, 0211), its
// retained notes MAM-PLAN-0310..0319 (NodeFile 0310), its graph warnings MAM-GRAPH-0203..0219
// (NodeFile 0203, 0204). A kind's resource is named "<node>/<kind's word><what>", like
// "<node>/file/etc/nginx/nginx.conf".
//
// NodeUser took MAM-PLAN-0145..0149, the observation problems MAM-PLAN-0212 and 0213, the retained
// note MAM-PLAN-0311 and the graph warning MAM-GRAPH-0206; its resources are "<node>/user/<name>".
//
// NodeArtifact ("<node>/artifact/usr/local/bin/node_exporter") takes MAM-PLAN-0155..0159, 0216 and
// 0217 (observation), 0313 (retained), MAM-GRAPH-0215 (0216..0219 kept for it), MAM-POL-0225 (0226
// kept for it).
package linux
