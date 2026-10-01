// Package deploy turns a cafaye project into a running deployment, and reports
// afterwards what is actually running.
//
// # Why caf drives Kamal rather than being Kamal
//
// Kamal owns the deploy. It builds the image, pushes it, installs Docker on a
// host that lacks it, boots kamal-proxy, performs a zero-downtime rolling
// rollout gated on a healthcheck, prunes the old release and can roll back.
// Reimplementing that in Go would be a second deploy engine that can disagree
// with the first about what a deploy is, and the disagreement would be
// discovered in production.
//
// So caf runs exactly one command that changes anything — `kamal setup` — and
// owns the part on either side of it: the refusal that should happen before it,
// and the report that has to be true after it.
//
// One correction worth making plainly, because it is commonly stated backwards:
// kamal is a Ruby gem and always has been. The precompiled binaries in its
// toolbox are kamal-proxy and kamal-secrets, and Kamal puts those on the
// *server*. So the consequence is narrower and clearer than "no Ruby is needed":
// the deployed service image never needs Ruby, and the machine you deploy *from*
// does. caf's preflight says so rather than letting a customer discover it as
// `kamal: command not found`.
//
// # `kamal setup`, and why not `kamal deploy`
//
// `setup` is "install Docker, boot the accessories, deploy". `deploy` is the last
// of those three alone. caf runs `setup` because the alternative is a customer
// who followed the recommended path onto a fresh VPS and got a service with no
// database — a connection-refused from inside the application, caused by a step
// caf chose not to take. `setup` skips accessories that already have a container
// (`Kamal::Cli::Accessory#boot`), so it is also the right command for every deploy
// after the first, and there is exactly one path to tell a customer about.
//
// # One pure function and one seam
//
// PlanFor is the whole decision: a Deployment and some Options in, an ordered
// list of Steps out, each with the exact argv and the reason it is there. It
// reads no file, opens no socket and runs no command, which is why every claim
// this package makes about what a deploy will do — that a dry run executes the
// read-only steps and neither of the two that change a server, that the argv a
// dry run prints is the argv a real deploy runs, that an environment is a config
// overlay rather than a flag — is assertable without a container runtime.
//
// Everything that touches the world is behind Runner, which is two methods wide.
// A fake that records argv is a dozen lines, so the whole command is testable.
//
// # What --dry-run means here, stated as a rule rather than a slogan
//
// A dry run executes every step that cannot change a server and prints every
// step that can, including the one that only runs after a failure. Nothing is
// written, no container is started, and the two read-only steps really do run —
// `kamal --version` and `kamal config` — because the configuration is an ERB
// template that only Kamal can evaluate. Printing a plan caf had to guess at
// would be a template with extra steps, and this repository already has those.
//
// The step names are constants (Preflight, Resolve, Deploy, State) rather than
// literals in the plan, because the report and the tests both name them and a
// rename that missed one would be a report about a step nothing runs.
//
// # The exposure refusal
//
// InspectResolved reads Kamal's resolved configuration and refuses any accessory
// that publishes its port to anything but loopback. An accessory's published
// port reaches every interface the host has, and the only thing between that and
// the internet is a host firewall this repository does not ship and cannot
// assume. Nothing needs the port: the accessory is on the `kamal` network under a
// stable name and app containers reach it by that name. See exposure.go for the
// measured mechanism, including the trap a loopback bind walks into.
package deploy
