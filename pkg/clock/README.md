# Simulation clock

OpenMeter uses the system clock by default. The billing simulation enables a paused,
externally controlled clock by setting `OPENMETER_SIMULATION_CLOCK_FILE` to a file on a
volume shared by the API and worker processes.

The API exposes simulation-only routes at `GET /__sim/clock`, `PUT /__sim/clock`, and
`POST /__sim/clock/advance`. The runner initializes the clock and advances it monotonically;
optimistic revisions reject stale writers. Only domain code using `pkg/clock.Now()` observes
simulation time. Infrastructure timeouts, health checks, and scheduling continue to use wall
time.
