# subsvc — سرویس سبک مدیریت و تست اشتراک (Subscription)

یک سرویس تک‌فایلی با Go (فقط کتابخانه‌ی استاندارد، صفر وابستگی خارجی) برای Debian 13 که:

- چند لینک اشتراک را نگه می‌دارد،
- فقط **با درخواست صریح شما** آن‌ها را دریافت و پردازش می‌کند (متن ساده یا Base64)،
- کانفیگ‌های تکراری را با **URI کامل و نرمال‌شده** (نه نام نمایشی) حذف می‌کند،
- هر کانفیگ را **واقعاً از تونل پروکسی خودش** تست می‌کند (`https://www.google.com/generate_204`)،
- و فقط کانفیگ‌های سالم را در خروجی می‌دهد.

هیچ دیتابیس، Docker، فریم‌ورک، زمان‌بند یا پردازش پس‌زمینه‌ای ندارد. اطلاعات در دو فایل JSON ذخیره می‌شود. مصرف RAM خود سرویس در حالت بیکار حدود ۸ مگابایت است (اندازه‌گیری‌شده).

## معماری و نحوه‌ی تست واقعی

پیاده‌سازی پروتکل‌های VLESS/VMess/Trojan/Shadowsocks (Reality، WebSocket، gRPC و ...) در Go خالص یعنی هزاران خط کد امنیتی که نگهداری آن سخت است. به‌جای آن، سرویس برای **هر تست** یک پردازش کوتاه‌عمر [Xray-core](https://github.com/XTLS/Xray-core) اجرا می‌کند:

```
subsvc ──(فایل کانفیگ موقت 0600)──► xray run   ← فقط 127.0.0.1:<پورت تصادفی> SOCKS5
   │                                    │
   └── HTTPS GET generate_204 ──SOCKS5──┘──► سرور پروکسی همان کانفیگ ──► google.com
```

- ترافیک تست **فقط** از SOCKS5 همان هسته عبور می‌کند؛ اتصال مستقیم هرگز استفاده نمی‌شود.
- هدف HTTPS است و گواهی آن سرتاسری (از داخل تونل) اعتبارسنجی می‌شود؛ پس کانفیگی که ترافیک را به جای دیگری منحرف کند نمی‌تواند تست را «پاس» کند.
- پاسخ `204` = موفق. Timeout، خطای اتصال، بستن تونل، یا هر وضعیت دیگر HTTP (حتی ریدایرکت) = شکست.
- حداکثر **۲ تست هم‌زمان** (سقف سخت؛ مقدار بیشتر در کانفیگ رد می‌شود).
- پس از هر تست، پردازش Xray کشته و فایل موقت پاک می‌شود. با توقف سرویس، تست در حال اجرا هم متوقف می‌شود.

### معنی فیلدهای نتیجه‌ی تست

| فیلد | معنی |
|---|---|
| `tested` | تست واقعاً انجام شد (کانفیگ‌های پشتیبانی‌نشده `false` می‌مانند) |
| `reachable` | پاسخ `204` از طریق همان پروکسی دریافت شد |
| `connect_ms` | زمان برقراری اتصال: از شروع تا آماده‌شدن تونل به مقصد (شامل SOCKS CONNECT و TLS handshake از داخل پروکسی) |
| `response_ms` | زمان دریافت پاسخ HTTP: از آماده‌شدن تونل تا رسیدن پاسخ |
| `latency_ms` | **زمان کل** درخواست موفق (`connect_ms + response_ms`). برای تست ناموفق `0` است |
| `status_code` | کد HTTP دریافتی، یا `0` اگر پاسخی نیامد |
| `error` | دلیل شکست (مثلاً `timeout after 5000ms`) |

زمان راه‌اندازی خود Xray در `latency_ms` حساب **نمی‌شود**؛ ولی ساخت اتصال به سرور پروکسی (که Xray در اولین درخواست انجام می‌دهد) حساب می‌شود، چون بخشی از تجربه‌ی واقعی کاربر است.

## پیش‌نیازها

- Debian 13، پردازنده‌ی دو‌هسته‌ای، ۲ گیگابایت RAM (کافی است)
- Go ≥ 1.22 فقط برای **ساخت** (Debian 13 نسخه‌ی 1.24 دارد)
- باینری `xray` فقط برای **تست** کانفیگ‌ها
- `ca-certificates` (به‌صورت پیش‌فرض روی Debian نصب است)

## ساخت

```bash
sudo apt update && sudo apt install -y golang-go make
cd subsvc
make build            # خروجی: bin/subsvc  (باینری استاتیک، حدود ۷ مگابایت)
# یا بدون make:
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/subsvc ./cmd/subsvc
```

ساخت برای سرور دیگر (کراس‌کامپایل): `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o subsvc ./cmd/subsvc` (برای ARM: `GOARCH=arm64`).

اجرای تست‌ها:

```bash
make test                                        # بدون Xray (هسته‌ی جعلی داخل تست)
XRAY_BIN=/usr/local/bin/xray make test           # با Xray واقعی (تست سرتاسری + اعتبارسنجی کانفیگ‌های تولیدشده)
```

## نصب روی Debian 13

### ۱) نصب Xray-core

```bash
sudo apt install -y curl unzip
curl -fsSL -o /tmp/xray.zip https://github.com/XTLS/Xray-core/releases/latest/download/Xray-linux-64.zip
# (برای ARM64: Xray-linux-arm64-v8a.zip). فایل .dgst کنار هر نسخه برای بررسی هش موجود است.
sudo unzip -o /tmp/xray.zip xray -d /tmp/xray-bin
sudo install -m 0755 /tmp/xray-bin/xray /usr/local/bin/xray
xray version
```

> نیازی به فعال‌کردن سرویس systemd خود Xray نیست؛ subsvc آن را فقط هنگام تست اجرا می‌کند.

### ۲) نصب subsvc

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin subsvc
sudo install -m 0755 bin/subsvc /usr/local/bin/subsvc

sudo install -d -m 0750 -o root -g subsvc /etc/subsvc
sudo install -m 0640 -o root -g subsvc deploy/config.example.json /etc/subsvc/config.json
sudo sed -i "s/CHANGE-ME-to-a-long-random-string/$(openssl rand -hex 24)/" /etc/subsvc/config.json

sudo install -m 0644 deploy/subsvc.service /etc/systemd/system/subsvc.service
sudo systemctl daemon-reload
sudo systemctl enable --now subsvc
```

بررسی:

```bash
systemctl status subsvc
journalctl -u subsvc -f
curl -s http://127.0.0.1:8080/healthz        # ok
```

داده‌ها در `/var/lib/subsvc` (`links.json`، `configs.json`) ذخیره می‌شوند و فایل‌ها `0600` هستند چون URL اشتراک‌ها معمولاً توکن محرمانه دارند. برای پشتیبان‌گیری همین دو فایل را کپی کنید.

## پیکربندی (`/etc/subsvc/config.json`)

همه‌ی کلیدها اختیاری‌اند؛ مقدار پیش‌فرض در جدول آمده. کلید ناشناخته (غلط املایی) هنگام شروع خطا می‌دهد.

| کلید | پیش‌فرض | توضیح |
|---|---|---|
| `listen` | `127.0.0.1:8080` | آدرس و پورت API |
| `data_dir` | `/var/lib/subsvc` | محل فایل‌های ذخیره |
| `auth_token` | خالی | اگر پر باشد همه‌ی درخواست‌ها (به‌جز `/healthz`) باید `Authorization: Bearer <token>` داشته باشند |
| `log_level` | `info` | `debug` / `info` / `warn` / `error` |
| `fetch_timeout_sec` | `20` | Timeout دریافت هر اشتراک (۱ تا ۳۰۰) |
| `fetch_concurrency` | `2` | دریافت هم‌زمان اشتراک‌ها (۱ تا ۴) |
| `max_body_bytes` | `10485760` | حداکثر حجم پاسخ اشتراک |
| `user_agent` | `subsvc/1.0` | User-Agent دریافت اشتراک |
| `test.url` | `https://www.google.com/generate_204` | آدرس تست |
| `test.timeout_ms` | `5000` | Timeout هر کانفیگ (میلی‌ثانیه) |
| `test.startup_timeout_ms` | `4000` | مهلت بالا‌آمدن هسته‌ی Xray |
| `test.concurrency` | `2` | تست هم‌زمان؛ فقط `1` یا `2` |
| `test.core_path` | `/usr/local/bin/xray` | مسیر Xray |

> **امنیت:** پیش‌فرض فقط روی `127.0.0.1` گوش می‌دهد. API فاقد TLS است؛ برای دسترسی از راه دور از تونل SSH (`ssh -L 8080:127.0.0.1:8080 server`) یا یک reverse proxy با TLS استفاده کنید و حتماً `auth_token` بگذارید. اگر روی آدرس غیر loopback بدون توکن اجرا شود، هشدار در لاگ ثبت می‌شود.

## API

متغیرها برای نمونه‌ها:

```bash
API=http://127.0.0.1:8080
TOKEN=...            # همان auth_token
H="Authorization: Bearer $TOKEN"
```

| متد و مسیر | کار |
|---|---|
| `GET /healthz` | سلامت (بدون توکن) |
| `GET /api/links` | فهرست لینک‌ها |
| `POST /api/links` | افزودن لینک `{"name":"...","url":"https://..."}` |
| `GET /api/links/{id}` | یک لینک |
| `PUT /api/links/{id}` | ویرایش `name` / `url` / `enabled` |
| `DELETE /api/links/{id}` | حذف لینک و کانفیگ‌هایی که فقط از آن آمده‌اند |
| `POST /api/refresh` | شروع به‌روزرسانی دستی. پارامترها: `link=ID`، `test=true\|false` (پیش‌فرض `true`) |
| `POST /api/test` | تست مجدد کانفیگ‌های ذخیره‌شده. پارامترها: `link=ID`، `only_untested=true` |
| `POST /api/cancel` | لغو کار در حال اجرا |
| `GET /api/status` | وضعیت کار جاری/آخرین کار، آمار کل و وضعیت هر لینک |
| `GET /api/configs` | کانفیگ‌ها همراه نتیجه‌ی تست (JSON). پارامترها: `reachable=true`، `link=ID` |
| `GET /api/output` | **خروجی نهایی متنی**: فقط کانفیگ‌های تست‌موفق، سریع‌ترین اول. پارامترها: `format=text\|base64`، `link=ID`، `limit=N` |

نمونه‌ها:

```bash
# افزودن لینک‌ها
curl -s -H "$H" -X POST $API/api/links -d '{"name":"main","url":"https://example.com/sub/abc"}'
curl -s -H "$H" -X POST $API/api/links -d '{"name":"backup","url":"https://example.org/sub/xyz"}'
curl -s -H "$H" $API/api/links

# دریافت + حذف تکراری + تست واقعی همه‌ی لینک‌های فعال (پاسخ 202؛ کار در پس‌زمینه اجرا می‌شود)
curl -s -H "$H" -X POST $API/api/refresh

# فقط دریافت و پردازش، بدون تست
curl -s -H "$H" -X POST "$API/api/refresh?test=false"

# فقط یک لینک
curl -s -H "$H" -X POST "$API/api/refresh?link=2779ad8f"

# پیگیری پیشرفت
curl -s -H "$H" $API/api/status

# تست مجدد فقط کانفیگ‌هایی که هنوز تست نشده‌اند
curl -s -H "$H" -X POST "$API/api/test?only_untested=true"

# لغو
curl -s -H "$H" -X POST $API/api/cancel

# خروجی نهایی (فقط سالم‌ها، سریع‌ترین اول)
curl -s -H "$H" $API/api/output > good.txt
curl -s -H "$H" "$API/api/output?format=base64&limit=50"   # فرمت Subscription (Base64)

# جزئیات نتیجه‌ی تست هر کانفیگ
curl -s -H "$H" "$API/api/configs?reachable=true"
```

نمونه‌ی پاسخ بخشی از `/api/configs`:

```json
{
  "key": "vless://b831381d-...@127.0.0.1:18443?security=none",
  "uri": "vless://b831381d-...@127.0.0.1:18443?security=none&type=tcp#vless-good",
  "name": "vless-good",
  "protocol": "vless",
  "sources": ["2779ad8f"],
  "test": {
    "tested": true, "reachable": true,
    "latency_ms": 142, "connect_ms": 98, "response_ms": 44,
    "status_code": 204, "tested_at": "2026-10-15T11:49:12Z"
  }
}
```

کدهای خطا: `400` ورودی نامعتبر، `401` توکن، `404` لینک ناموجود، `409` کار دیگری در حال اجراست (یا لینک تکراری)، `500` خطای داخلی. بدنه‌ی خطا همیشه `{"error":"..."}` است.

## رفتار در برابر خطا

- هر لینک جداگانه دریافت می‌شود؛ Timeout، `HTTP 403/500`، پاسخ HTML، پاسخ خالی یا بدون هیچ کانفیگ معتبر فقط برای همان لینک ثبت می‌شود (`last_fetch.error`) و بقیه ادامه می‌دهند.
- **اگر دریافت یک لینک شکست بخورد، کانفیگ‌های ذخیره‌شده‌ی قبلی‌اش پاک نمی‌شوند.**
- خطای هر لینک بدون URL ثبت می‌شود تا توکن محرمانه در لاگ/پاسخ لو نرود.
- سطرهای خراب داخل یک اشتراک شمارش می‌شوند (`invalid`) و نادیده گرفته می‌شوند.
- خطای هر تست (Timeout، اتصال، کرش هسته) فقط به همان کانفیگ نسبت داده می‌شود.
- نوشتن فایل‌ها اتمیک است (فایل موقت + rename)، پس قطع برق فایل نیمه‌نوشته نمی‌سازد.

## حذف تکراری‌ها (Deduplication)

کلید یکتایی، URI کامل **پس از نرمال‌سازی** است و نام نمایشی (`#...` یا `ps`) در آن دخیل نیست:

- scheme و host حروف کوچک، UUID حروف کوچک، `%`-encoding یکسان
- ترتیب پارامترهای query مهم نیست؛ پارامتر خالی یا برابر با پیش‌فرض (`type=tcp`، `encryption=none`، `security=tls` در trojan و ...) حذف می‌شود
- `vmess://` ابتدا Base64 JSON را باز و بدون `ps` مقایسه می‌کند
- `ss://` در هر سه فرم (SIP002 با Base64، متن ساده، و فرم قدیمی Base64 کامل) یکسان می‌شود
- تکراری بودن هم داخل یک اشتراک و هم بین اشتراک‌ها بررسی می‌شود؛ هر کانفیگ یک بار ذخیره می‌شود و `sources` فهرست لینک‌های حاوی آن است.

## پروتکل‌های پشتیبانی‌شده برای تست

| پروتکل | انتقال‌ها | امنیت |
|---|---|---|
| `vless://` | tcp، ws، grpc، httpupgrade، xhttp، kcp (بدون header/seed) | none، tls، reality |
| `vmess://` (JSON Base64 یا URI) | همان‌ها | none، tls |
| `trojan://` | همان‌ها | tls (پیش‌فرض)، reality، none |
| `ss://` | tcp | — (شامل متدهای SS-2022) |

کانفیگ‌هایی که Xray نمی‌تواند اجرا کند (`hysteria2://`، `tuic://`، `ssr://`، `wireguard://`، پلاگین Shadowsocks، vmess روی quic و ...) **ذخیره و تکراری‌زدایی می‌شوند** ولی چون تست‌پذیر نیستند، `tested=false` دارند و هرگز وارد خروجی نمی‌شوند؛ دلیل در `test.error` آمده است.

### نکات مهم نسخه‌ی جدید Xray

با Xray ‏26.3.27 بررسی شد که این قابلیت‌ها از هسته **حذف شده‌اند** و subsvc برای آن‌ها کانفیگ نادرست نمی‌سازد:

- `allowInsecure`: نادیده گرفته می‌شود و تست با اعتبارسنجی عادی گواهی انجام می‌شود. کانفیگی که به گواهی خودامضا وابسته است شکست می‌خورد و خطا این را ذکر می‌کند.
- انتقال `h2/http` و تنظیمات `header/seed` در `kcp`: «پشتیبانی‌نشده» علامت می‌خورد.

اگر نسخه‌ی قدیمی‌تر Xray دارید همین رفتار محافظه‌کارانه اعمال می‌شود.

## عیب‌یابی

| نشانه | علت/راه‌حل |
|---|---|
| همه‌ی تست‌ها `start core: ... no such file` | `test.core_path` را درست کنید / Xray را نصب کنید |
| `core exited: ...` | کانفیگ آن لینک برای Xray نامعتبر است؛ متن خطای Xray در خود پیام هست |
| همه `timeout after 5000ms` | اینترنت خروجی سرور یا مسدودبودن `test.url`؛ با `curl -I https://www.google.com/generate_204` از خود سرور بررسی کنید |
| `409 a job is already running` | `GET /api/status` را ببینید یا `POST /api/cancel` بزنید |
| `data is corrupt` هنگام شروع | فایل مشخص‌شده را اصلاح یا حذف کنید (سرویس عمداً آن را بی‌صدا دور نمی‌ریزد) |

لاگ‌ها: `journalctl -u subsvc`. برای جزئیات هر تست `"log_level": "debug"` بگذارید.

## ساختار پروژه

```
cmd/subsvc/            نقطه‌ی ورود، راه‌اندازی و خاموشی تمیز
internal/config/       خواندن و اعتبارسنجی پیکربندی
internal/model/        نوع‌های داده‌ی مشترک
internal/proxy/        parse لینک‌ها، نرمال‌سازی/کلید یکتایی، ساخت outbound برای Xray
internal/subscription/ دریافت HTTP، تشخیص متن/Base64، حذف تکراری داخل اشتراک
internal/store/        ذخیره‌ی JSON اتمیک (لینک‌ها و کانفیگ‌ها)
internal/tester/       اجرای هسته، تست از طریق SOCKS5، اندازه‌گیری زمان
internal/service/      کارهای refresh/test (یک کار در هر لحظه، لغو‌پذیر)
internal/api/          هندلرهای HTTP (net/http)
deploy/                subsvc.service و config.example.json
```
