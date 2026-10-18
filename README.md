# subsvc — Subscription Collector & Tester

A lightweight self-hosted service that collects proxy subscription links, removes duplicates, **actually tests each config through its own tunnel**, and serves only the working ones as a clean subscription URL for v2rayNG / NekoBox / Streisand.

Running as a Docker container — no Go, no Xray, no manual setup required.

---

## What it does

- Stores multiple subscription URLs
- Fetches and parses configs (plain text or Base64)
- Removes duplicates by normalized URI (not display name)
- Tests each config **through its own proxy tunnel** against `https://www.google.com/generate_204` (general) and `https://www.youtube.com/generate_204` (YouTube), plus an optional capped speed test
- Only configs that return `204` are kept in the output
- Serves a Base64 subscription URL ready for any client
- Includes a web UI for managing links, monitoring progress, and getting results

---

## Requirements

- **Docker Desktop** (Windows/macOS) or **Docker Engine + Compose plugin** (Linux)
- ~500 MB free disk space
- Internet connection

That's it. No Go, no Xray, no Python needed on the host.

---

## Quick start

```bash
# 1. Unzip this folder somewhere
# 2. Open a terminal in that folder
# 3. Run:

docker compose up -d
```

That's it. Wait ~10 seconds, then open:

- **UI:** http://localhost:9090
- **API:** http://localhost:8080
- **Health check:** http://localhost:8080/healthz

---

## First-time setup (via UI)

1. Open **http://localhost:9090**
2. Go to **"لینک‌ها" (Links)** tab
3. Paste your subscription URL (e.g. `https://example.com/sub/abc`) and click **افزودن (Add)**
4. Go back to **"داشبورد" (Dashboard)** and click **"دریافت + تست همه" (Fetch + Test all)**
5. Wait for the test to finish (progress bar updates every 2 seconds)
6. Go to **"خروجی" (Output)** tab and click **نمایش (Show)**
7. Click **کپی همه (Copy all)** or **دانلود (Download)**

---

## Adding the subscription to v2rayNG / NekoBox

Go to the **"Subscription"** tab in the UI. You'll see a URL like:

```
http://<YOUR-PC-IP>:9090/sub
```

Copy it and add it in your client:

- **v2rayNG:** ☰ → Subscription → Add
- **NekoBox:** Group → New group → Subscription
- **Streisand:** Subscription → Add

Every time you tap "Update" in the client, it fetches the latest working configs.

---

## Configuration

There are two kinds of settings, with a clear precedence.

**1. Deployment settings** — environment variables (`SUBSVC_*`), set in `.env` / `docker-compose.yml`. Changing them needs `docker compose up -d`.
Precedence: **ENV > optional legacy `config.json` (only `listen`, `data_dir`, `auth_token`, `log_level`, `test.core_path`) > defaults**.

| Variable | Default | Meaning |
|---|---|---|
| `SUBSVC_AUTH_TOKEN` | *(required in compose)* | Bearer token for the whole API (`/healthz` excepted). Also `SUBSVC_AUTH_TOKEN_FILE` |
| `SUBSVC_LISTEN` | `127.0.0.1:8080` (`0.0.0.0:8080` in Docker) | API bind address. Non-loopback without a token → a random token is generated and stored in `<data_dir>/auth_token` (0600) |
| `SUBSVC_DATA_DIR` | `/var/lib/subsvc` (`/data` in Docker) | Links, results and `settings.json` |
| `SUBSVC_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `SUBSVC_CORE_PATH` | `/usr/local/bin/xray` | Proxy core binary |
| `SUBSVC_MAX_TEST_CONCURRENCY` | `20` | Hard upper limit the UI cannot exceed |
| `SUBSVC_MAX_SPEED_BYTES` | `8388608` | Hard upper limit for the speed-test size |
| `SUBSVC_ALLOW_PRIVATE_TEST_URLS` | `false` | Allow private/loopback test URLs (local testing only) |
| `SUBSVC_PORT` / `SUBSVC_UI_PORT` | `8080` / `9090` | Published host ports (compose) |
| `SUBSVC_UI_OUTPUT_NAME`, `SUBSVC_UI_OUTPUT_NUMBERED` | | Naming of configs in `/sub` |

**2. Operational settings** — edited in the UI (**Settings** tab) or `GET/PUT /api/settings`, validated by the backend, saved in `<data_dir>/settings.json` and kept across restarts. They are *not* read from `config.json` or ENV. A running test keeps the settings it started with; changes apply to the next run.

- test concurrency (default 10), general/YouTube/speed timeouts
- general URL (`https://www.google.com/generate_204`) and YouTube URL (`https://www.youtube.com/generate_204`)
- speed test on/off and max bytes per config (default 1 MB), speed concurrency
- success criteria (require general / YouTube / minimum speed, max latency) and ranking (`score` / `latency` / `speed`, weights)

### How testing works

Every probe goes through the config's **own tunnel** (a short-lived Xray process with one outbound; direct connections are blocked). Per config, **general**, **YouTube** and **speed** are judged independently, each with its own latency and error kind (`timeout`, `dns`, `tls`, `connect`, `http_status`, ...). The speed test only runs for configs whose connectivity checks pass. A config is *eligible* for `/sub` according to the success criteria in Settings, and ranking is never by latency alone. Configs tested before this version have no YouTube verdict and need a retest if YouTube is required.

### Updating Docker

```bash
cp .env.example .env            # first time only; set SUBSVC_AUTH_TOKEN (openssl rand -hex 24)
docker compose pull && docker compose up -d        # published images
docker compose up -d --build                       # or build from this source
```

Existing volumes keep working (ownership is fixed once by the entrypoint).

---

## Common commands

```bash
docker compose ps                  # status
docker compose logs -f subsvc      # live logs
docker compose logs -f subsvc-ui
docker compose up -d               # apply .env changes
docker compose down                # stop everything
docker compose up -d               # start again
docker compose pull && docker compose up -d   # update images
```

---

## Data & backup

All links and configs are stored in a Docker volume named `downloads_subsvc-data`.

Backup:

```bash
docker run --rm -v downloads_subsvc-data:/data -v ${PWD}:/backup alpine \
  tar czf /backup/subsvc-backup.tar.gz -C /data .
```

Restore:

```bash
docker run --rm -v downloads_subsvc-data:/data -v ${PWD}:/backup alpine \
  tar xzf /backup/subsvc-backup.tar.gz -C /data
```

---

## Security notes

- The API requires `Authorization: Bearer <token>` (except `/healthz`); docker-compose refuses to start without `SUBSVC_AUTH_TOKEN`. Tokens are never logged.
- The UI (port 9090) does **not** add the server token to `/api/*`; paste the token into the UI once (kept in your browser). Only `/sub` is public, so v2rayNG can fetch it. Still, don't expose 9090 to the internet unless you accept a public `/sub`.
- Settings changes need the token; test URLs are validated (http/https, no credentials, no loopback/private addresses).
- Resources: bounded concurrency, one short-lived core per test with capped memory/CPU, cancellable runs, cleanup of temp dirs; compose sets CPU/memory limits.

## Troubleshooting

| Symptom | Fix |
|---|---|
| UI shows "خطای اتصال" | `docker compose ps` — check `subsvc` container is Up |
| All tests show `timeout` | From inside the container, check: `docker exec subsvc wget -qO- https://www.google.com/generate_204` |
| `Address already in use` on port 8080 or 9090 | Another program uses the port. Change it in `docker-compose.yml` |
| ENV changes don't apply | Run `docker compose up -d` (settings made in the UI apply immediately) |
| v2rayNG can't fetch `/sub` | Open http://localhost:9090 in a browser and check the Subscription tab shows a working URL |

---

## Project structure

```
.
├── docker-compose.yml
├── .env.example             ← copy to .env, set SUBSVC_AUTH_TOKEN
├── README.md
├── subsvc/                  ← Go backend source
└── subsvc-ui/               ← Python UI + HTML
```

---

## License

Provided as-is. Use at your own risk.

---

<div dir="rtl">

# subsvc — جمع‌آور و تست‌کننده‌ی اشتراک

یک سرویس سبک و self-hosted که لینک‌های اشتراک پروکسی را جمع می‌کند، تکراری‌ها را حذف می‌کند، **هر کانفیگ را واقعاً از تونل خودش تست می‌کند**، و فقط کانفیگ‌های سالم را به‌صورت یک لینک اشتراک تمیز برای v2rayNG / NekoBox / Streisand ارائه می‌دهد.

به‌صورت کانتینر Docker اجرا می‌شود — نیازی به نصب Go، Xray یا هر چیز دیگری روی سیستم شما نیست.

---

## چه کاری انجام می‌دهد

- چندین لینک اشتراک را نگه می‌دارد
- کانفیگ‌ها را دریافت و پردازش می‌کند (متن ساده یا Base64)
- تکراری‌ها را بر اساس URI نرمال‌شده حذف می‌کند (نه نام نمایشی)
- هر کانفیگ را **واقعاً از تونل پروکسی خودش** در برابر `https://i.ytimg.com/generate_204` تست می‌کند
- فقط کانفیگ‌هایی که کد `204` برمی‌گردانند در خروجی می‌مانند
- یک لینک Subscription به‌صورت Base64 آماده برای هر کلاینت ارائه می‌دهد
- یک رابط کاربری وب برای مدیریت لینک‌ها، پیگیری پیشرفت و دریافت نتایج دارد

---

## پیش‌نیازها

- **Docker Desktop** (ویندوز/مک) یا **Docker Engine + Compose plugin** (لینوکس)
- حدود ۵۰۰ مگابایت فضای خالی
- اتصال اینترنت

همین. نیازی به Go، Xray یا Python روی سیستم شما نیست.

---

## راه‌اندازی سریع

```bash
# ۱. این پوشه را در جایی از حالت فشرده خارج کن
# ۲. یک ترمینال در همان پوشه باز کن
# ۳. اجرا کن:

docker compose up -d
```

همین. حدود ۱۰ ثانیه صبر کن، بعد باز کن:

- **UI:** http://localhost:9090
- **API:** http://localhost:8080
- **سلامت سرویس:** http://localhost:8080/healthz

---

## راه‌اندازی اولیه (از طریق UI)

1. برو به **http://localhost:9090**
2. تب **«لینک‌ها»** را باز کن
3. لینک اشتراکت را پیست کن (مثلاً `https://example.com/sub/abc`) و **افزودن** را بزن
4. برگرد به **«داشبورد»** و **«دریافت + تست همه»** را بزن
5. صبر کن تا تست تمام شود (نوار پیشرفت هر ۲ ثانیه آپدیت می‌شود)
6. برو به تب **«خروجی»** و **نمایش** را بزن
7. **کپی همه** یا **دانلود** را بزن

---

## افزودن Subscription به v2rayNG / NekoBox

برو به تب **«Subscription»** در UI. یک URL مثل این می‌بینی:

```
http://<IP-کامپیوتر-تو>:9090/sub
```

کپی کن و در کلاینت اضافه کن:

- **v2rayNG:** ☰ → Subscription → Add
- **NekoBox:** Group → New group → Subscription
- **Streisand:** Subscription → Add

هر بار که در کلاینت **Update** بزنی، آخرین کانفیگ‌های سالم را می‌گیرد.

---

## پیکربندی

دو دسته تنظیمات داریم و اولویت‌ها مشخص است.

**۱. تنظیمات استقرار** — متغیرهای محیطی `SUBSVC_*` در `.env` یا `docker-compose.yml`؛ تغییر با `docker compose up -d` اعمال می‌شود.
اولویت: **ENV ‏>‏ فایل قدیمی `config.json` (فقط `listen`، `data_dir`، `auth_token`، `log_level`، `test.core_path`) ‏>‏ پیش‌فرض**.

| متغیر | پیش‌فرض | معنی |
|---|---|---|
| `SUBSVC_AUTH_TOKEN` | *(در compose الزامی)* | توکن Bearer برای کل API (به‌جز `/healthz`) |
| `SUBSVC_LISTEN` | `127.0.0.1:8080` (در Docker ‏`0.0.0.0:8080`) | آدرس bind. اگر loopback نباشد و توکن نباشد، توکن تصادفی ساخته و در `<data_dir>/auth_token` ذخیره می‌شود |
| `SUBSVC_DATA_DIR` | `/var/lib/subsvc` (در Docker ‏`/data`) | لینک‌ها، نتایج و `settings.json` |
| `SUBSVC_LOG_LEVEL` | `info` | سطح لاگ |
| `SUBSVC_CORE_PATH` | `/usr/local/bin/xray` | مسیر هسته |
| `SUBSVC_MAX_TEST_CONCURRENCY` | `20` | سقف سخت هم‌زمانی که UI نمی‌تواند از آن بیشتر کند |
| `SUBSVC_MAX_SPEED_BYTES` | `8388608` | سقف سخت حجم تست سرعت |
| `SUBSVC_PORT` / `SUBSVC_UI_PORT` | `8080` / `9090` | پورت‌های منتشرشده روی میزبان |

**۲. تنظیمات عملیاتی** — از تب **تنظیمات** در UI (یا `GET/PUT /api/settings`) ویرایش می‌شوند، در بک‌اند اعتبارسنجی و در `<data_dir>/settings.json` ذخیره می‌شوند و بعد از ری‌استارت می‌مانند. از `config.json` یا ENV خوانده نمی‌شوند. تست در حال اجرا با تنظیمات زمان شروعش ادامه می‌دهد و تغییر روی اجرای بعدی اثر دارد.

- هم‌زمانی تست (پیش‌فرض ۱۰) و timeout های عمومی/یوتیوب/سرعت
- آدرس تست عمومی (`https://www.google.com/generate_204`) و یوتیوب (`https://www.youtube.com/generate_204`)
- روشن/خاموش بودن تست سرعت و حداکثر حجم هر کانفیگ (پیش‌فرض ۱ مگابایت)
- معیار موفقیت (الزام عمومی / یوتیوب / حداقل سرعت / حداکثر تأخیر) و رتبه‌بندی (`score` / `latency` / `speed`)

### نحوه‌ی تست

هر تست از **تونل خود کانفیگ** عبور می‌کند (یک پروسه‌ی کوتاه‌عمر Xray؛ اتصال مستقیم مسدود است). وضعیت **عمومی**، **یوتیوب** و **سرعت** مستقل از هم و هرکدام با تأخیر و نوع خطای خودشان (`timeout`، `dns`، `tls`، `connect`، `http_status` ...) ثبت می‌شوند. تست سرعت فقط برای کانفیگ‌هایی اجرا می‌شود که تست‌های اتصال را رد کرده‌اند. رتبه‌بندی فقط بر اساس تأخیر نیست. نتایج قدیمی وضعیت یوتیوب ندارند و در صورت الزام یوتیوب باید دوباره تست شوند.

### به‌روزرسانی Docker

```bash
cp .env.example .env            # فقط بار اول؛ SUBSVC_AUTH_TOKEN را تنظیم کن (openssl rand -hex 24)
docker compose pull && docker compose up -d        # ایمیج‌های منتشرشده
docker compose up -d --build                       # یا ساخت از سورس
```

---

## دستورات رایج

```bash
docker compose ps                  # وضعیت
docker compose logs -f subsvc      # لاگ زنده
docker compose logs -f subsvc-ui
docker compose restart subsvc      # اعمال تغییرات config
docker compose down                # توقف همه
docker compose up -d               # دوباره اجرا
docker compose pull && docker compose up -d   # بروزرسانی ایمیج‌ها
```

---

## داده‌ها و پشتیبان‌گیری

همه‌ی لینک‌ها و کانفیگ‌ها در یک volume داکر به اسم `downloads_subsvc-data` ذخیره می‌شن.

پشتیبان:

```bash
docker run --rm -v downloads_subsvc-data:/data -v ${PWD}:/backup alpine \
  tar czf /backup/subsvc-backup.tar.gz -C /data .
```

بازگردانی:

```bash
docker run --rm -v downloads_subsvc-data:/data -v ${PWD}:/backup alpine \
  tar xzf /backup/subsvc-backup.tar.gz -C /data
```

---

## نکات امنیتی

- API به `Authorization: Bearer <token>` نیاز دارد (به‌جز `/healthz`)؛ compose بدون `SUBSVC_AUTH_TOKEN` بالا نمی‌آید. توکن هرگز لاگ نمی‌شود.
- UI توکن سرور را به `/api/*` اضافه نمی‌کند؛ توکن را یک‌بار در UI وارد کن (در مرورگر می‌ماند). فقط `/sub` عمومی است تا v2rayNG بتواند بگیرد.
- تغییر تنظیمات نیاز به توکن دارد؛ آدرس‌های تست اعتبارسنجی می‌شوند (بدون credential و آدرس خصوصی/loopback).
- منابع: هم‌زمانی محدود، یک هسته‌ی کوتاه‌عمر برای هر تست با سقف حافظه/CPU، لغو و پاک‌سازی کامل؛ compose محدودیت CPU/RAM دارد.

## عیب‌یابی

| نشانه | راه‌حل |
|---|---|
| UI «خطای اتصال» نشان می‌دهد | `docker compose ps` — چک کن کانتینر `subsvc` Up باشد |
| همه‌ی تست‌ها `timeout` می‌شوند | از داخل کانتینر چک کن: `docker exec subsvc wget -qO- https://www.google.com/generate_204` |
| `Address already in use` روی پورت 8080 یا 9090 | برنامه‌ی دیگری پورت را گرفته. در `docker-compose.yml` عوضش کن |
| تغییرات config اعمال نمی‌شود | `docker compose restart subsvc` را فراموش کردی |
| v2rayNG نمی‌تواند `/sub` را بگیرد | http://localhost:9090 را در مرورگر باز کن و تب Subscription را ببین |

---

## ساختار پروژه

```
.
├── docker-compose.yml
├── .env.example             ← کپی به .env و تنظیم SUBSVC_AUTH_TOKEN
├── README.md
├── subsvc/                  ← سورس Go backend
└── subsvc-ui/               ← Python UI + HTML
```

---

## مجوز

همان‌طور که هست ارائه می‌شود. مسئولیت استفاده با خودت است.

</div>