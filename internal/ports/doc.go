// Package ports is caf's port registry: the block it publishes from, the
// reservation that holds a port for the life of a session, and the prober that
// finds out whether something already has it.
//
// # Why this exists, and why it is not "ask the daemon"
//
// The obvious design is to let the container runtime arbitrate: publish a port
// and read the tiebreak off the failure. Container against container that works,
// and the failure is loud — `Bind for 127.0.0.1:15002 failed: port is already
// allocated`.
//
// Container against a **plain host process** it does not work. Measured on this
// machine (OrbStack): a host listener on 127.0.0.1:15004, then
// `docker run -p 127.0.0.1:15004:80` — the container **starts**, `docker ps`
// shows the mapping, every connection goes to the host process, and nothing
// errors. The container is up and unreachable.
//
// That is worse than the failure the daemon does catch, because the failure it
// catches is loud and this one is a green-looking stack. A worker whose suite
// needs Postgres on 15000 gets a connection to *something*, and the error lands
// in the code under test at three in the morning in a wave of eight parallel
// workers. So:
//
//   - the reservation is **held**, not probed. A probe answers at one instant and
//     the collision happens after it.
//   - the prober asks **both** loopback families, always. An IPv4-only prober
//     calls a port free while something is genuinely listening on it.
//   - the prober never sets **SO_REUSEPORT**. See probe.go; the trap is worse
//     than "two binds both succeed".
//   - the registry never picks a port **outside the block**. The sprawl this
//     exists to stop (15001, 16001, 21101, 55432) is what a port outside the
//     block looks like the morning after.
//
// What the hold does *not* close is stated in the report rather than hidden: a
// non-caf process that grabs a port between the probe and `up` is caught by the
// daemon on Linux and not caught on OrbStack. Holding the port with a caf lock
// keeps the caf fleet correct; it cannot make the daemon arbiter honest.
//
// # One block, one lock per port
//
// The block is 15000-15999, which is the range a parallel fleet of workers can
// all look at and recognise as ours. Each port in it has its own advisory lock in
// the ledger, so a hundred reservations are a hundred locks and not one queue —
// and so a crashed caf strands nothing, because flock is owned by the open file
// description and the kernel drops it when the process dies.
package ports
