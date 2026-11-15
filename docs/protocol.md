# PRO-BLADE Station: read-only HID telemetry

Verified protocol revision: USB VID 0x4791 PID 0x2064, firmware 2.0.1.2.
Discovery uses /sys/class/hidraw/*/device/uevent. hidraw number is not stable.

Protocol recovered by static inspection of the official updater downloaded from:
https://downloads.sandisk.com/downloads/pro-blade-station_fw_utility_win.zip
Archive SHA256: 6d3923fe808c52a800f383d5eef630a764e99a9b64f289d6927288a86fd16d6f
The application contains a .NET ResourceSet byte-array `app`, with a four-byte size prefix; remaining image maps at 0x08005000.
Telemetry uses documented Linux HID GET_INPUT_REPORT operations and positively identified GET handlers. Subsequent explicitly authorized tests verified report-6 fan control and automatic restore. No thermal-table, firmware, reset or bootloader writes were performed.

## Compact GET_INPUT_REPORT

- ID 2, length 6: report byte, five signed temperature bytes. MCU reads station sensor 0, then sensor channels 1..4.
- ID 3, length 3: report byte, cached RPM as unsigned LE16.
- ID 1, length 4: internal status bytes, meaning not completely decoded.
- ID 4, length 2: slot/input bits, not used for conclusions.
- ID 9, length 17: device serial string; excluded from reports.
- ID 8 GET_INPUT_REPORT alone did not reply; full GET request below works.

## Report 8 GET request/reply

64-byte output packet: [8, payload_length, checksum, request_type, command, ...payload], zero padding.
request_type=0 GET, checksum=sum(bytes[3:3+payload_length]) modulo 256.
Replies: byte0=8, byte3=1, byte4=command; checksum covers same range. Validate report ID, command, length and checksum.

Allowlisted requests:
- GET_FW command0: [8,2,0,0,0]; reply bytes5..8 contain version2,0,1,2.
- GET_STATUS command1: [8,2,1,0,1]; reply uint16le at5=RPM, signed int16le at7,9,11,13,15=station and four slot temperature channels. Bytes17..19 are auxiliary status.
- GET_THERMAL_TABLE command10: [8,3,10+index,0,10,index], index0..6. Reply bytes6..25 contain four rows x five channel thresholds. Reply byte5 is not an echoed index. No writes to thermal tables were performed.

Firmware evidence (Thumb image base 0x08005000):
- 0x08009b72 builds compact report2: calls temp0 getter 0x0800903c, then 0x08008de0 for channels1..4.
- 0x08009bbe builds compact report3 from uint16 at RAM0x2000010e.
- 0x0800adc8 updates same RAM0x2000010e from 0x0800ac20.
- 0x0800869c shell `rpm` calls 0x0800ac20 and prints `[INFO] FAN RPM: %d`.
- 0x0800ac20 combines nonzero tachometer counter samples; uses constants4000 and60000 for RPM calculation.
- 0x0800b7f0 GET_STATUS response calls same RPM function0x0800ac20, then five temperature getters; no control-write calls in this GET branch.
- 0x0800b742 dispatch uses request type at offset3 and command atoffset4.
- 0x0800ba42 GET_THERMAL_TABLE reads index from requestbyte5, limits index modulo7, copies20 bytes from RAM table to response.
- 0x0800bc5c is distinct thermal-table SET branch and was NOT sent.
- 0x0800903c retrieves signed byte sensor temperature over I2C.
- 0x08008de0 retrieves the slot's reported temperature or falls back to its board sensor depending on slot status.

Compact RPM report examples: 03ff0f =>4095RPM; 030000 =>0RPM.
Verified full-status fixture: fan0RPM, station56C, slots49/48/43/41C. Slot order is controller channel order; physical bay numbering has not been independently verified.
The Go exporter polls status on a bounded background schedule and refreshes firmware and tables less frequently. Physical bay numbering must not be inferred from the sensor index.

## Opt-in report-6 fan control

The HID descriptor declares report ID 6 with two payload bytes. Firmware dispatcher 0x0800b6e2 reads `(payload[0] & 1, payload[1])` and calls 0x0800af6c. Manual mode stores its flag at RAM 0x20000110, duty at RAM 0x20000111, and writes timer duty via 0x0800d000. Automatic restore clears that flag and uses the factory controller's target from 0x20000108. Packet `[6, 1, duty]` requests manual duty, `[6, 0, 0]` restores automatic control. Duty is encoded as `round(percent * 255 / 100)`; the existing timer period uses 0xfe. There is no independently verified PWM readback field. Actual RPM comes from GET_STATUS.

Authorized live testing observed ~1620 RPM at byte51 (20%) for 20 minutes, with falling temperatures and zero HID errors. Lower values did not provide stable low RPM and could stall. Profiles remain unchanged. The optional controller bounds its inputs, reissues commands, verifies measured RPM and renews a watchdog lease; it does not expose arbitrary HID writes.
