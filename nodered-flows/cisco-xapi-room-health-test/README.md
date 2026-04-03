# Room Health Test

Node-RED flow for automated nightly AV room health testing. Connects to Cisco codecs via JSXAPI (WebSocket), runs audio diagnostics, reads display telemetry, and reports structured results to Zabbix via the `history.push` API.

## What it does

1. Receives an HTTP trigger from Zabbix (POST `/test/:systemName/health`)
2. Connects to the room's Cisco codec via JSXAPI over WSS
3. Checks occupancy (PeoplePresence) -- skips if room is in use
4. Runs `Audio Diagnostics Start` -- plays an impulse through speakers, measures response on every mic
5. Reads display telemetry (Video Output) -- EDID sync, power state, connected displays
6. Disconnects and evaluates pass/fail
7. Pushes all results to Zabbix trapper items via `history.push` JSON-RPC

## Prerequisites

- `jsxapi` npm package installed in the Node-RED runtime
- Cisco codecs with xAPI WebSocket access enabled
- Zabbix API token with permission to push history
- Trapper items configured on the target Zabbix hosts (see below)

## Installation

### 1. Install jsxapi in Node-RED

Add `jsxapi` to the npm install step in your Node-RED Dockerfile:

```dockerfile
RUN npm install --no-fund --no-update-notifier jsxapi
```

Or install at runtime via the Node-RED palette manager.

### 2. Import the flow

Copy the contents of `flow.json` and import into Node-RED via Menu > Import > Clipboard.

### 3. Configure environment variables

The flow reads these from Node-RED's environment or global context:

| Variable | Required | Description |
|----------|----------|-------------|
| `ZABBIX_API_URL` | Yes | Zabbix JSON-RPC endpoint (e.g., `http://zabbix-web:8080/api_jsonrpc.php`) |
| `ZABBIX_API_TOKEN` | Yes | Zabbix API auth token with history push permission |

Set these in `settings.js` under `functionGlobalContext`, or as flow-level environment variables in the Node-RED editor.

### 4. Allow self-signed codec certificates

Cisco codecs typically use self-signed TLS certificates. Set this environment variable on the Node-RED container:

```
NODE_TLS_REJECT_UNAUTHORIZED=0
```

## Zabbix configuration

### Host macros

Set these macros on each system host (manually or via reconciler):

| Macro | Example | Description |
|-------|---------|-------------|
| `{$OG_CODEC_IP}` | `10.1.2.100` | Codec IP address |
| `{$OG_CODEC_AUTH}` | `YWRtaW46cGFzc3dvcmQ=` | Base64-encoded `username:password` |

### HTTP agent item (initiator)

Create one HTTP agent item on the template to trigger the test:

| Field | Value |
|-------|-------|
| Name | Nightly health test: initiate |
| Type | HTTP agent |
| Key | `og.synth.health.initiate` |
| URL | `http://nodered:1880/workflow/test/{$OG_SYSTEM_NAME}/health` |
| Request body | `{"codec_ip":"{$OG_CODEC_IP}","codec_auth":"{$OG_CODEC_AUTH}","zbx_host":"{HOST.HOST}"}` |
| Request type | POST |
| Update interval | Schedule as needed (e.g., `0;h24/02:00-06:00`) |

### Trapper items (results)

Create these trapper items on the same template:

| Key | Type | Value type | Description |
|-----|------|------------|-------------|
| `og.synth.health.status` | Trapper | Character | `passed`, `failed`, `error`, or `skipped_occupied` |
| `og.synth.health.duration_ms` | Trapper | Numeric (unsigned) | Test duration in milliseconds |
| `og.synth.health.log` | Trapper | Text | Full event log as JSON array |
| `og.synth.health.audio.channels_passed` | Trapper | Numeric (unsigned) | Mic channels that passed diagnostics |
| `og.synth.health.audio.channels_total` | Trapper | Numeric (unsigned) | Total mic channels tested |
| `og.synth.health.display.connected` | Trapper | Numeric (unsigned) | Number of connected displays |
| `og.synth.health.display.edid_ok` | Trapper | Numeric (unsigned) | 1 if all connected displays have valid EDID |
| `og.synth.health.display.power_ok` | Trapper | Numeric (unsigned) | 1 if at least one display reports power on |

### Suggested triggers

| Expression | Severity | Description |
|------------|----------|-------------|
| `last(/template/og.synth.health.status)="failed"` | Average | Health test failed |
| `last(/template/og.synth.health.status)="error"` | Warning | Health test encountered an error |
| `nodata(/template/og.synth.health.status,93600)` | Warning | No test result in 26 hours |
| `last(/template/og.synth.health.audio.channels_passed)<last(/template/og.synth.health.audio.channels_total)` | Average | Not all audio channels passed |
| `last(/template/og.synth.health.display.edid_ok)=0` | Average | Display EDID sync failure |

## Request format

```
POST /workflow/test/{systemName}/health
Content-Type: application/json

{
    "codec_ip": "10.1.2.100",
    "codec_auth": "YWRtaW46cGFzc3dvcmQ=",
    "zbx_host": "system-host-name"
}
```

Response: `202 Accepted` (test runs asynchronously, results pushed to Zabbix when complete).

## Manual testing

```bash
curl -X POST http://localhost/workflow/test/test-room/health \
  -H 'Content-Type: application/json' \
  -d '{"codec_ip":"10.1.2.100","codec_auth":"YWRtaW46cGFzc3dvcmQ=","zbx_host":"test-host"}'
```

Check the Node-RED debug panel for the result object.
