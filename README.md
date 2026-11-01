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
After a successful startup, transport failures trigger reconnects every
`-modbus.scan` without an attempt limit. Modbus exception replies are logged and
polling continues with other registers. `-modbus.pause` (default `50ms`) applies
between requests.

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
