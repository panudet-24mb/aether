# Tuya BLE test vectors

`vectors.json` is written by `gen_vectors.py`, which runs the byte-level functions of the MIT-licensed ha_tuya_ble reference, with the
bleak and Home Assistant plumbing removed and a fixed IV injected where the reference draws a random one. The
references are pinned to these commits:

- PlusPlus-ua/ha_tuya_ble `6037ac5a04ceb23a36d1b88e2303aa1da7fdbe83` (2023-07-09)
- ha-tuya-ble/ha_tuya_ble `40899aeff5f1bcb63aca26fba4ee59b76e101681` (2026-09-30)

Files used: `custom_components/tuya_ble/tuya_ble/{tuya_ble.py,security.py,const.py}`.

## Coverage

| Section | Reference function |
|---|---|
| `crc16`, `varint` | `_calc_crc16`, `_pack_int` / `_unpack_int` |
| `keys` | `TuyaBLESecurityMaterial`: legacy and `sec_key` login and session keys, and the flags |
| `frames` | `_build_packets` with a fixed IV, then reassembly (`_notification_handler`) and `_parse_input`. Cases: device info (plain, FD50 `00 f3`, `sec_key`, and with the advertised protocol 3 and 4 in the first fragment), pair, status, DPS v3 and v4, an ack, and a long frame of 7 fragments |
| `pairing_request` | `_build_pairing_request` |
| `dps` | `TuyaBLEDataPoint._get_value` + `_encode_datapoints` and `_parse_datapoints`, for length sizes 1 and 2, covering every type and enum widths 1, 2 and 4 |
| `receive` | Payload layouts of `RECEIVE_DP`, `RECEIVE_TIME_DP` (both timestamp types), `RECEIVE_DP_V4` (with and without an ack) and `RECEIVE_TIME_DP_V4`, with the acks from `_handle_command_or_response` |
| `time` | The TIME1 and TIME2 answers for 2026-01-01 07:00 at UTC+7 |
| `device_info` | The byte offsets `_handle_command_or_response` reads from the `DEVICE_INFO` answer |
| `adverts` | `_decode_advertisement_data` (uuid decryption); the manufacturer data is built by encrypting with the same key and IV |

Sign reports (`RECEIVE_SIGN_DP` 0x8004 and `RECEIVE_SIGN_TIME_DP` 0x8005) are not in the vectors. The reference reads
their DPs from the flags byte, which this package does not copy; see `docs/platform/tuya-ble.md`.

The `testdata/fuzz` directory contains fuzz seeds, including a regression seed for enum indices of 128 and above.

## Regenerating

Developer-only; CI reads the JSON. With Python 3.9 or later:

```sh
python3 -m venv /tmp/tb && /tmp/tb/bin/pip install pycryptodome==3.23.0
/tmp/tb/bin/python gen_vectors.py > vectors.json
```

`gen_vectors.py` carries the functions copied from the references with their MIT notice. Every random input (keys,
srand, IV) is pinned, so the output is byte-for-byte reproducible. The script checks that every frame round-trips
through the reference parser before printing it.
