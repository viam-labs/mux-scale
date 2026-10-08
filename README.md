# mux-scale

A [Viam](https://www.viam.com) module that reads weight from one or more
[SparkFun Qwiic Scale - NAU7802](https://www.sparkfun.com/products/15242) load
cell amplifiers. Because every NAU7802 answers at the same I2C address (0x2A),
multiple scales must sit behind a
[SparkFun Qwiic Mux (TCA9548A)](https://www.sparkfun.com/products/16784); this
module selects the right mux channel around every transaction so that each
scale appears as its own Viam sensor component.

I2C access uses [periph.io](https://periph.io) on Linux.

> The `viam-labs:mux-scale` namespace is a placeholder until the module is
> published. It is defined once in `muxscale/config.go` and `meta.json`.

## Wiring

```
host I2C bus ──► Qwiic Mux (0x70) ──┬─ ch0 ─► Qwiic Scale (0x2A) ─► load cell
                                    ├─ ch1 ─► Qwiic Scale (0x2A) ─► load cell
                                    └─ ...
```

A single scale may also be connected directly to the bus; omit the mux
attributes in that case. Up to eight muxes (0x70–0x77) may share one bus. The
NAU7802 DRDY pin is not used; the module polls the conversion-ready flag.

## Model viam-labs:mux-scale:nau7802

One component per load cell. Components on the same bus share a single bus
handle inside the module; the bus is held only for the few microseconds of
each register transaction, so all scales convert in parallel.

### Attributes

| Attribute | Type | Default | Description |
|---|---|---|---|
| `i2c_bus` | string | required | I2C bus, e.g. `"1"` or `"/dev/i2c-1"` |
| `mux_channel` | int | none | Mux channel 0–7 the scale is wired to. Omit for a scale wired directly to the bus. |
| `mux_address` | int | 112 (0x70) | Mux address 112–119 (0x70–0x77). Requires `mux_channel`. |
| `i2c_address` | int | 42 (0x2A) | NAU7802 address |
| `gain` | int | 128 | PGA gain: 1, 2, 4, 8, 16, 32, 64 or 128 |
| `sample_rate` | int | 80 | Conversions per second: 10, 20, 40, 80 or 320 |
| `ldo_voltage` | float | 3.3 | Load cell excitation voltage: 2.4, 2.7, 3.0, 3.3, 3.6, 3.9, 4.2 or 4.5 |
| `samples` | int | 8 | Conversions averaged per reading |
| `zero_offset` | float | none | Raw reading of the empty scale |
| `calibration_factor` | float | none | Raw counts per unit of weight; non-zero |
| `allow_negative_weight` | bool | false | Report weights below zero instead of clamping to 0 |
| `tare_on_start` | bool | false | Measure the zero offset when the component starts. Cannot be combined with `zero_offset`. |

A reading takes about `samples / sample_rate` seconds (100 ms by default).

### Example configuration

```json
{
  "components": [
    {
      "name": "cell0",
      "api": "rdk:component:sensor",
      "model": "viam-labs:mux-scale:nau7802",
      "attributes": { "i2c_bus": "1", "mux_channel": 0 }
    },
    {
      "name": "cell1",
      "api": "rdk:component:sensor",
      "model": "viam-labs:mux-scale:nau7802",
      "attributes": {
        "i2c_bus": "1",
        "mux_channel": 1,
        "zero_offset": 142305,
        "calibration_factor": 418.72
      }
    }
  ]
}
```

### Readings

| Key | Type | Description |
|---|---|---|
| `raw` | float | Averaged signed 24-bit ADC value |
| `samples` | int | Number of conversions averaged |
| `calibrated` | bool | Whether a `calibration_factor` is known |
| `weight` | float | `(raw - zero_offset) / calibration_factor`. Present only when `calibrated` is true. The unit is whatever unit the known weight was given in during calibration. |

### DoCommand

| Command | Effect |
|---|---|
| `{"tare": true}` | Average the current reading and use it as the zero offset |
| `{"calibrate": {"known_weight": 500}}` (or `{"calibrate": 500}`) | With a known mass on the scale and a zero offset known, compute `calibration_factor` |
| `{"get_calibration": true}` | Report `zero_offset`, `calibration_factor` and `calibrated` |
| `{"set_calibration": {"zero_offset": 142305, "calibration_factor": 418.72}}` | Apply values directly; either key may be omitted |
| `{"reset": true}` | Re-run the NAU7802 power-up sequence, including its internal calibration |

Every command returns the current calibration state.

### Calibrating a scale

1. With nothing on the scale, send `{"tare": true}`.
2. Place a known mass on the scale and send `{"calibrate": {"known_weight": <mass>}}`.
3. Copy the returned `zero_offset` and `calibration_factor` into the
   component's attributes. **Values applied with DoCommand live only in
   memory** and are lost when the component restarts or is reconfigured.

### Status

`Status` returns the effective configuration plus the calibration state and an
`afe_calibration_error` flag. The NAU7802's internal analog calibration is
retried once at start-up; if it fails twice the component still starts, logs a
warning, and sets the flag. `{"reset": true}` retries it.

## Development

```sh
make lint          # go vet (+ golangci-lint when installed)
make test          # go test -race ./...
make build         # bin/mux-scale (CGO_ENABLED=0)
make module.tar.gz # package for `viam module upload`
```

Nushell equivalents:

```nu
make lint; make test; make build
```

The driver (`nau7802/`) is hardware-independent and unit-tested against an
in-memory register emulation (`nau7802/nau7802test`); the mux and bus logic is
tested against an emulated bus (`internal/i2cbus/i2cbustest`) that only exposes
a device while its mux channel is selected.

### Running locally against viam-server

Build for the target, copy `bin/mux-scale` to the machine, and add a local
module to the machine config:

```json
{
  "modules": [
    { "type": "local", "name": "mux-scale", "executable_path": "/home/pi/mux-scale" }
  ]
}
```

`i2cdetect -y 1` should show only the mux (0x70) while the module is idle, both
before and after readings, because every transaction leaves all channels
disabled.
