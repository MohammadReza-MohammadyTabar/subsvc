# subsvc — Subscription Collector & Tester

A lightweight self-hosted service that collects proxy subscription links, removes duplicates, **actually tests each config through its own tunnel**, and serves only the working ones as a clean subscription URL for v2rayNG / NekoBox / Streisand.

Running as a Docker container — no Go, no Xray, no manual setup required.

---

## What it does

- Stores multiple subscription URLs
- Fetches and parses configs (plain text or Base64)
- Removes duplicates by normalized URI (not display name)
- Tests each config **through its own proxy tunnel** against `https://i.ytimg.com/generate_204`
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

Edit `config.json` in this folder, then:

```bash
docker compose restart subsvc
```

Key options:

| Key | Default | Meaning |
|---|---|---|
| `listen` | `0.0.0.0:8080` | API bind address (don't change inside Docker) |
| `auth_token` | `""` | If set, all API calls need `Authorization: Bearer <token>` |
| `log_level` | `info` | `debug` / `info` / `warn` / `error` |
| `fetch_timeout_sec` | `20` | Timeout for downloading each subscription |
| `test.url` | `https://i.ytimg.com/generate_204` | URL used to test configs |
| `test.timeout_ms` | `5000` | Per-config timeout in milliseconds |
| `test.concurrency` | `10` | How many configs tested at once (raise on strong machines) |

After editing, restart the container:

```bash
docker compose restart subsvc
```

---

## Common commands

```bash
docker compose ps                  # status
docker compose logs -f subsvc      # live logs
docker compose logs -f subsvc-ui
docker compose restart subsvc      # apply config changes
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

- By default `auth_token` is **empty**, meaning anyone who can reach port 8080 can control the service. If you expose it to a network you don't trust, **set a token**.
- Port 9090 (UI) has no built-in auth. It proxies requests to the backend using the token from the server side, so v2rayNG can fetch `/sub` without credentials. **Don't expose port 9090 to the internet.**
- The service is designed to run on localhost or behind a VPN / SSH tunnel.

---

## Troubleshooting

| Symptom | Fix |
|---|---|
| UI shows "خطای اتصال" | `docker compose ps` — check `subsvc` container is Up |
| All tests show `timeout` | From inside the container, check: `docker exec subsvc wget -qO- https://www.google.com/generate_204` |
| `Address already in use` on port 8080 or 9090 | Another program uses the port. Change it in `docker-compose.yml` |
| Config changes don't apply | You forgot `docker compose restart subsvc` |
| v2rayNG can't fetch `/sub` | Open http://localhost:9090 in a browser and check the Subscription tab shows a working URL |

---

## Project structure

```
.
├── docker-compose.yml
├── config.json              ← edit this
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

فایل `config.json` در همین پوشه را ویرایش کن، بعد:

```bash
docker compose restart subsvc
```

گزینه‌های مهم:

| کلید | پیش‌فرض | معنی |
|---|---|---|
| `listen` | `0.0.0.0:8080` | آدرس bind (داخل Docker دست نزن) |
| `auth_token` | `""` | اگه پر باشه، همه‌ی درخواست‌ها به `Authorization: Bearer <token>` نیاز دارن |
| `log_level` | `info` | `debug` / `info` / `warn` / `error` |
| `fetch_timeout_sec` | `20` | Timeout دانلود هر اشتراک |
| `test.url` | `https://i.ytimg.com/generate_204` | URL تست کانفیگ‌ها |
| `test.timeout_ms` | `5000` | Timeout هر کانفیگ به میلی‌ثانیه |
| `test.concurrency` | `10` | چند کانفیگ هم‌زمان تست بشن (روی سیستم قوی بالاتر ببر) |

بعد از ویرایش، کانتینر را ری‌استارت کن:

```bash
docker compose restart subsvc
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

- به‌صورت پیش‌فرض `auth_token` **خالی** است، یعنی هر کسی که به پورت 8080 دسترسی دارد می‌تواند سرویس را کنترل کند. اگر آن را روی شبکه‌ای که به آن اعتماد نداری باز می‌کنی، **حتماً توکن بگذار**.
- پورت 9090 (UI) احراز هویت ندارد. درخواست‌ها را با توکن سمت سرور به backend پروکسی می‌کند تا v2rayNG بتواند بدون credential از `/sub` استفاده کند. **پورت 9090 را روی اینترنت باز نکن.**
- این سرویس برای اجرا روی localhost یا پشت VPN / SSH tunnel طراحی شده.

---

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
├── config.json              ← این را ویرایش کن
├── README.md
├── subsvc/                  ← سورس Go backend
└── subsvc-ui/               ← Python UI + HTML
```

---

## مجوز

همان‌طور که هست ارائه می‌شود. مسئولیت استفاده با خودت است.

</div>