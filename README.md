# P1 Meter (Go) – HomeWizard P1 dashboard and logger

One self-contained binary (no dependencies, nothing loaded from the internet) for a Raspberry Pi 3B+.
Talks to the HomeWizard P1 meter over **API v2**, serves a web UI, and logs to plain CSV files.

## 1. Build (on your PC, needs Go 1.22+)

```sh
./build.sh            # 64-bit Raspberry Pi (aarch64)  <- default
./build.sh armv7      # 32-bit Raspberry Pi OS (if `uname -m` on the Pi says armv7l)
./build.sh pc         # this computer, for trying it out
```

Everything you need ends up in **`dist/`**:

```
dist/
├── p1meter                    the program (static binary for the chosen target)
├── p1meter.service            systemd unit, starts it at boot and restarts it if it crashes
└── data/
    ├── config.example.json    example of the file with your meter address + token
    └── logs/
        ├── master/            master log: one CSV per day (filled automatically)
        └── user/              your own logs (filled automatically)
```

Optional: `PI_USER=myname ./build.sh` if your Pi user is not `pi` (it is written into the service file).
The web UI is embedded in the binary, so after changing anything in `web/`, run `./build.sh` again.

## 2. Your meter address and token

You already have a working token from the Deno app, and **it works here unchanged**. In API v2 every token has the same full access to all endpoints; the name (`local/p1-deno-app`) is just a label. Nothing needs re-pairing, and both apps can run side by side.

```sh
cp dist/data/config.example.json dist/data/config.json
# edit dist/data/config.json: put in your meter's IP ("host") and your token
```

`userName` and `pairedAt` are informational only. Or skip this and pair from the UI (Device → Connection → *Pair*, then press the button on the meter); that creates a new user `local/p1-go-app`.

## 3. Install on the Pi (first time)

```sh
# on your PC, from the project folder:
ssh pi@<pi-address> 'mkdir -p ~/p1meter'
rsync -av dist/ pi@<pi-address>:~/p1meter/

# on the Pi:
chmod +x ~/p1meter/p1meter
sudo cp ~/p1meter/p1meter.service /etc/systemd/system/
sudo systemctl enable --now p1meter
```

Open `http://<pi-address>:8001`. Check on it with `systemctl status p1meter` or `journalctl -u p1meter -f`.

**Updating later** (keeps your config and all logs, because `data/` is excluded):

```sh
./build.sh
rsync -av --exclude data/ dist/ pi@<pi-address>:~/p1meter/
ssh pi@<pi-address> 'sudo systemctl restart p1meter'
```

## 4. Logging: how it works

- **Global tracking** (Logs tab) is the background log that Live and History read from. **It is off until you start it**, so a fresh install records nothing and does not poll the meter. To start it, go to **Logs → Global tracking settings → Start global tracking…**:
  1. The app asks the meter for a test reading and **switches off every field your meter has no data for** (they stay disabled and show "no data").
  2. You can untick more fields and choose the interval (1 s … 1 h, or custom seconds). *Test again* repeats the test.
  3. *Start tracking* begins recording: one CSV per day in `data/logs/master/`, containing **only the ticked fields as columns**.

  Afterwards the same card has *Save changes* and *Stop tracking*. Saving new fields rewrites **today's** file to exactly the ticked columns (today's data of columns you removed is dropped; older days are never touched). Each new day starts a new file with the current columns.
- **Live tracking** (Live tab) is off by default, so the Pi does not poll the meter faster than your global interval unless you ask. Pick an interval (1–30 s) and a maximum time (5 min … 6 h or custom) and press *Start live tracking*. The page updates every interval until the time is up or you press *Stop*, then it stops by itself. Every session is saved as its own log (marked "live session") on the Logs page. *Read once* asks the meter a single time.
- **Your own logs:** Logs → *Start a new log*: a name, an interval (1 s … 1 h), an automatic stop (never, 5, 15, 30, 45 min, 1, 2, 3, 4, 6, 12 h, or custom minutes) and a note. Run as many as you like; each is a CSV in `data/logs/user/`. *View* shows its charts; *Stop*, *Resume*, *Download CSV* and *Delete* are next to it. A log records the fields selected in Global tracking when it starts (all fields if global tracking was never set up). Its file is `data/logs/user/<name>-<YYYYMMDD>-<HHMMSS>.csv`, for example `washing-machine-20261031-143205.csv`; live sessions are called `live-<timestamp>.csv`.
- **Survives reboots:** with `systemctl enable` the app starts at boot, global tracking continues (if you started it), and every log that was running is resumed automatically (appending to the same file).
- **Missed time is shown, not hidden.** While the Pi is off, or the app or the meter connection is down, nothing can be recorded. Those periods appear as **hatched areas** on the charts, bars for periods with incomplete data are faded (the tooltip shows the data coverage), and lines are broken instead of drawn through. Energy and gas totals stay correct, because they come from the meter's own counters: whatever was used during a gap is spread evenly over the gap. The Pi has no real-time clock; after a power cut the app waits until the clock has synced and never writes rows out of order.

## 5. Looking at the data: panels (History and every log's page)

History and each log's *View* page are made of **panels**. A panel is a chart you define yourself: a name, a chart type and the fields to show. The panels are stored on the server (`data/panels.json`), so every browser and both pages show the same set. Starting set: Power, Electricity, Gas and Phase current.

- **Add / edit / delete:** *+ Add panel*, or *Edit* on a panel. The editor has a live preview that uses the data on screen. ↑ ↓ move a panel, *Reset to defaults* restores the starting set.
- **Chart types** (the editor shows a small picture of each): line, area, step line, **line + variability band** (the shaded lowest…highest range per bucket, now for any measurement), bars, stacked bars, **pie** and **donut**. Width (half / full) and height are also yours to choose.
- **Calculated fields** (meter readings only show the running total, so these show what changed):
  - *Change between points*: how much a counter grew from one point to the next, e.g. gas used.
  - *Rate per hour*: that change divided by the time: gas flow in m³/h, or import/export power in kW from the meter's counters.
  - *Difference A − B*: any two fields with the same unit, e.g. Power L1 − Power L2.
  - Ready-made ones (gas flow, water flow, power from the import/export meter) are in the field list; the builder under it makes your own. They follow the **Detail** setting: gas only updates every few minutes, so use 5 min or coarser to get a smooth gas flow.
- **Values only** panels show big numbers instead of a chart: pick the fields and whether to show the last value, average, lowest, highest or total of the period on screen. Any other chart can show the same numbers in a row above it (*Show the values above the chart*), so one panel can hold values and a chart.
- **Pie / donut:** every field is a slice. Measurements give their average over the period, usage and meter readings the amount used. All slices must have the same unit (e.g. kWh with kWh), for instance import tariff 1 vs tariff 2, or power per phase.
- **Value axis:** per panel, *Automatic*, *Starts at zero* or *Fits the data (not from zero)*.
- **Names:** under **Device → Names** say what your tariffs are called (e.g. “Low (night)” and “High (day)”); that name is used everywhere (charts, tiles, field lists). You can rename any other field there too. Inside a panel's editor you can override the name of a field for that chart only.
- **Fields** (up to 8 per panel, drawn on top of each other; *Calculated* is described below):
  - *Measurements*: total power, power / voltage / current per phase, frequency.
  - *Usage per period*: electricity imported / fed back (and per tariff), gas, water used in each time bucket.
  - *Meter readings*: the meter counters themselves (import, export, gas, water, tariff).
- **Mixed units:** fields with different units (say power and gas) are shown with **each field scaled to 0–100 %** so you can compare their shapes. Hover to see the real values. You can also force this for same-unit fields. (There are no dual axes on purpose.)
- **Periods:** 10 min, 1 h, 6 h, 24 h, Yesterday, Today, 7 days, 30 days, any single day (‹ ›) or a date range.
- **Zoom:** drag across any chart (on a phone: swipe sideways across it, or use the ‹ − + › buttons above each chart; tap a chart to see its values); the app asks the server again, so zooming shows finer detail, down to every logged sample. *Back* and *Show whole period* undo it.
- **Detail** (Auto / Raw / 1 min / 5 min / 15 min / 1 h / 1 day) sets the line averages and the size of the usage buckets. You cannot get finer detail than the interval the data was logged at.
- Click legend entries to show or hide a series (remembered in the browser). *Table* shows the numbers. A panel only shows fields that are in your CSV files, so untracked fields appear as "no data".

Every folder under `-data` is created automatically when it is needed (also if you delete it while the app runs), so `dist/data/` can be empty or missing.

## Flags

| Flag | Default | |
|---|---|---|
| `-addr` | `:8001` | listen address (`127.0.0.1:8001` = this machine only) |
| `-data` | `./data` | folder with `config.json` and `logs/` |
| `-auth` | – | `user:password` for HTTP basic auth (or env `P1_AUTH`) |

## What it does

- **Live**: last reading, optional live tracking sessions, power chart, today's totals compared with yesterday, cost estimate, meter readings.
- **History**: see section 5, plus peak, average, standby load, estimated cost and change versus the previous period.
- **Logs**: global tracking settings, your own logs, live sessions.
- **Device** (everything the v2 API offers): device info, Wi-Fi/uptime, LED brightness, cloud on/off, API v1 on/off, identify, reboot, user list and delete, Plug-In Battery mode/permissions/charge-to-full, raw telegram, raw JSON explorer.

## Files the app creates

```
data/config.json                      meter address, token (chmod 600), prices
data/logs/master/YYYY-MM-DD.csv       global tracking, one file per day (only once started)
data/logs/master/.rollup/             cache of finished days (safe to delete)
data/logs/user/<name>-<timestamp>.csv your own logs and live sessions; index.json lists them
data/panels.json                      your panel definitions (History and log pages)
data/config.json also holds your names ("labels") and prices
```

CSV columns: `timestamp` (RFC 3339 with offset) plus the fields you selected: power (total + L1–L3), voltage, current, frequency, import/export meter readings (total, T1, T2), gas, water, tariff. All 20 fields every 5 s take about 2.7 MB/day (~1 GB/year); fewer fields or a longer interval take proportionally less. **Back up `data/`** and you have everything.

## Things to know

- The meter's HTTPS certificate is not verified (same as `--unsafely-ignore-certificate-errors` in the Deno version); the token is therefore only as safe as your LAN.
- Without `-auth`, anyone on your network can open the UI, including reboot/delete-user buttons. Set `-auth` (or `Environment=P1_AUTH=user:pass` in the service file) if that matters.
- Times in charts follow the browser's time zone; "yesterday/today" follow the Pi's. Set the Pi's zone with `sudo raspi-config`.

## Development

```sh
go test ./...
go run ./cmd/mockmeter -gen ./data/logs/master -days 9   # fake history
go run ./cmd/mockmeter -listen :8443 &                    # fake meter (token ABCDEF0123456789ABCDEF0123456789)
go run . -addr :8001      # then set the address to localhost:8443 and pair
```
