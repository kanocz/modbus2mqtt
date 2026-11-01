# modbus2mqtt
simple modbus to mqtt bridge for Home Assistant

created just for fun to control my ventilation/recuperation unit which has single and multi bit options

## Command-line flags

Pass values as `-flag value` or `-flag=value`. Duration flags accept units, for
example `50ms`, `10s`, or `5m`.

| Flag | Default | Description |
| --- | --- | --- |
| `-config` | `modbus2mqtt.json` | Path to the JSON configuration containing registers, MQTT settings, and Home Assistant discovery settings. |
| `-modbus.connect` | `tcp://192.168.1.21:502` | Modbus endpoint URL. For example, `tcp://host:port` for Modbus TCP or `rtuovertcp://host:port` for RTU-over-TCP. |
| `-modbus.timeout` | `5s` | Modbus I/O timeout. Must be positive. |
| `-modbus.scan` | `10s` | Wait after each polling cycle and between Modbus connection attempts. A cycle's read time is added to this interval. Must be positive. |
| `-modbus.pause` | `50ms` | Pause before each Modbus read or write request, including requests made by commands. Must be nonnegative; `0` disables the pause. |
| `-modbus.startup-attempts` | `3` | Total Modbus connection attempts at startup, including the first attempt. Must be at least `1`. Reconnects after startup have no attempt limit. |
| `-modbus.read-attempts` | `3` | Total attempts per read after a timeout, using the same connection. Must be at least `1`; `1` disables read retries. Writes are never retried. |
| `-modbus.batch-size` | `125` | Maximum number of adjacent holding/input registers per read. Allowed range: `1`–`125`; `1` disables batching. |
| `-mqtt.broker` | Empty | MQTT broker address, such as `tcp://localhost:1883`. A nonempty value overrides `mqtt.broker` in the configuration. |
| `-mqtt.username` | Empty | MQTT username. A nonempty value overrides `mqtt.username` in the configuration. |
| `-mqtt.password` | Empty | MQTT password. A nonempty value overrides `mqtt.password` in the configuration. |
| `-mqtt.timeout` | `5s` | MQTT connection and write timeout, wait limit for subscribe/publish operations, and retry delay for failed subscriptions/publications. Must be positive. |
| `-mqtt.resync-interval` | `5m` | Minimum interval between full discovery and cached-state resends requested by MQTT reconnects. Must be positive. |
| `-h`, `-help` | — | Print command-line help and exit without connecting. |

Empty MQTT override flags keep the corresponding configuration values. To use
an empty username or password, clear that field in the JSON configuration.

## Running the bridge

For an RTU-over-TCP gateway, use `-modbus.connect rtuovertcp://host:port`.
At startup, `-modbus.startup-attempts` (default `3`, minimum `1`) limits the total
number of Modbus connection attempts, including the first attempt. Failed attempts
are separated by `-modbus.scan` (default `10s`); after the last failure the bridge
exits with an error. Configuration errors are not retried. This option applies to
Modbus only; an initial MQTT connection failure still exits immediately.
After a read timeout, the bridge sends the same request again on the same
connection, up to `-modbus.read-attempts` total attempts (default `3`: one initial
send and two retries). Each attempt uses `-modbus.timeout`; `-modbus.pause`
(default `50ms`) applies before every request, including retries. Retries do not
wait for `-modbus.scan`. This applies to holding registers, input registers,
coils, and the read step of read-modify-write commands; writes are never retried.
After the last read timeout or another transport error, the connection is closed
and reconnects are attempted every `-modbus.scan` without an attempt limit.
Modbus exception replies are not retried as timeouts; polling continues with
other registers.

Holding/input entries that are consecutive in the configuration and have adjacent
ascending addresses are read together, up to `-modbus.batch-size` registers.
Holding and input registers use separate requests; unconfigured address gaps and
daily-only entries that are not due are not included. For example, holding
registers 20, 21, and 22 can be read with one request and decoded separately.
Coil groups already use range reads. If a batch returns an illegal-address or
illegal-value exception, the bridge falls back to individual reads for that
batch in the current polling cycle. For a controller with a smaller read limit,
lower `-modbus.batch-size`, or set it to `1` to disable batching.

RTU-over-TCP has no transaction ID: a late response cannot be distinguished from
the response to a repeated identical request. Retries help with lost frames, but
do not guarantee recovery from delayed or overlapping responses.

MQTT command subscriptions are restored on every connection. Discovery and the
latest cached states are also resent after reconnection, at most once per
`-mqtt.resync-interval` (default `5m`).

Before running this example, set `mqtt.broker`, `mqtt.username`, and
`mqtt.password` in [configs/salda.json](configs/salda.json) for your MQTT broker,
or supply the corresponding command-line overrides. The bundled MQTT settings
are examples. Replace the Modbus address below with your gateway's address.

```sh
./modbus2mqtt -config configs/salda.json \
  -modbus.connect rtuovertcp://192.168.1.21:502 \
  -modbus.startup-attempts 3 \
  -modbus.read-attempts 3 \
  -modbus.batch-size 125 \
  -mqtt.resync-interval 5m
```

Reconnects inside that interval share one deferred full resend. Changed readings,
pending failed publications, command subscriptions, and availability updates do
not wait for the full resend. MQTT uses retained `online`/`offline` availability
and a retained offline Last Will; discovery links entities to that topic. The
availability reports the bridge's MQTT presence, not the health of every Modbus
register. Daily readings are cached for resending without extra device reads.

Commands run in receive order. Up to 100 commands can wait in the queue; further
commands are rejected with a log message. Failed Modbus writes are reported and
are not automatically repeated, since a timed-out write may already have reached
the device.

Configuration is validated at startup: names must be unique after normalization,
bit positions and widths must fit a 16-bit register, and input registers cannot
be writable. Coil groups must contain names and span at most 2000 addresses;
split wider groups. Only fields marked `writable: true` accept commands. Numeric
commands must fit the configured width/sign, and `x10` accepts steps of `0.1`.
Use `homeAssistantUnit` for an explicit unit of measurement. Registers containing
only `daily: true` fields are skipped for 24 hours after a successful read.
Mixed registers are still read for their non-daily fields.

Run checks with `go test -race ./...` and `go vet ./...`. Tests use local TCP
simulators and do not connect to a real MQTT broker or Modbus device.
