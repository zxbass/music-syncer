# music-syncer

CLI-синхронизатор музыкальной библиотеки между **Яндекс.Музыкой** и **Spotify**.
Написан на Go, работает в обе стороны.

```
Яндекс.Музыка  ──►  Spotify
      ▲                │
      └────────────────┘
```

## Что умеет

| Данные | Яндекс → Spotify | Spotify → Яндекс | Примечание |
|---|---|---|---|
| Плейлисты | ✅ | ✅ | порядок треков сохраняется, повторный запуск не создаёт дубликаты |
| Любимые треки | ✅ | ✅ | Яндекс «Мне нравится» ↔ Spotify Liked Songs |
| Любимые/отслеживаемые артисты | ✅ | ✅ | Яндекс «Мне нравится» у артистов ↔ Spotify Followed Artists |
| Экспорт в JSON | ✅ | ✅ | бэкап плейлистов, лайков и артистов |

История прослушиваний намеренно **не синхронизируется**: публичные API не умеют
её записывать, а чтение только создаёт лишнюю нагрузку.

Дополнительно:

- нечёткое сопоставление треков между каталогами: ISRC, нормализация названий,
  транслитерация кириллицы, длительность, защита от live/remix/acoustic версий;
- инкрементальный и идемпотентный перенос: состояние хранится локально,
  повторный запуск только добавляет недостающее;
- `--dry-run`, подробный отчёт в JSON, список несопоставленных треков
  с ближайшими кандидатами;
- безопасный `--mirror`: удаление лишних треков из целевого плейлиста
  автоматически пропускается, если хотя бы один исходный трек не сопоставлен.

## Важные ограничения

### Spotify Web API в development mode

С февраля 2026 года у приложений в development mode действуют ограничения:

- владелец приложения должен иметь активную подписку **Spotify Premium**;
- development mode рассчитан на 5 пользователей;
- лимит выдачи поиска — 10 результатов (клиент это учитывает);
- содержимое плейлиста можно читать, только если вы владелец или соавтор;
  чужие/отслеживаемые плейлисты помечаются как `read-only` и не переносятся;
- часть эндпоинтов переименована (`/playlists/{id}/items`, `PUT /me/library`).
  Клиент использует новые эндпоинты и автоматически откатывается на старые,
  если аккаунт работает в extended quota mode.

Если Spotify отвечает `403` на поиск, приложение работает в урезанном режиме.
Тогда перенос без поиска невозможен — понадобится extended quota mode.

### Яндекс.Музыка

Официального публичного API нет. Проект использует те же HTTP-эндпоинты, что и
клиент Яндекс.Музыки, с вашим личным OAuth-токеном. Эндпоинты могут меняться без
предупреждения. Используйте на свой риск и не передавайте токен третьим лицам.

### Почему история не переносится

История не переносится и не экспортируется: её нельзя записать обратно ни в
Яндекс, ни в Spotify, а чтение только нагружает API. Инструмент работает
только с плейлистами, любимыми треками и артистами.

## Требования

- Go 1.26+ (см. `go.mod`);
- аккаунт Яндекс.Музыки с токеном;
- Spotify-приложение в dashboard и аккаунт владельца с Premium (для dev mode).

## Установка

### Linux и macOS

Из исходников:

```bash
git clone https://github.com/zxbass/music-syncer.git
cd music-syncer
make build
./music-syncer --help
```

Без Makefile:

```bash
go build -trimpath -o music-syncer ./cmd/music-syncer
```

Установка в GOPATH/bin:

```bash
go install github.com/zxbass/music-syncer/cmd/music-syncer@latest
```

### Windows (PowerShell)

Требуется Go 1.26+ и Git (или скачанный ZIP-архив репозитория).
`make` не нужен — используйте `go build`:

```powershell
git clone https://github.com/zxbass/music-syncer.git
cd music-syncer
go build -trimpath -o music-syncer.exe .\cmd\music-syncer
.\music-syncer.exe --help
```

Установка в `%USERPROFILE%\go\bin`:

```powershell
go install github.com/zxbass/music-syncer/cmd/music-syncer@latest
```

После этого добавьте `%USERPROFILE%\go\bin` в `PATH` и запускайте
`music-syncer.exe` из любого каталога.

Во всех примерах ниже бинарник называется `music-syncer`. В Windows это
`music-syncer.exe`, а из текущего каталога его нужно запускать так:

```powershell
.\music-syncer.exe --help
```

Если программа уже добавлена в `PATH`, префикс `.\` не нужен.

## Готовые релизы

Готовые бинарники публикуются на странице
[GitHub Releases](https://github.com/zxbass/music-syncer/releases).

| Платформа | Архитектура | Файл |
|---|---|---|
| Linux | x86_64 | `music-syncer_<version>_linux_amd64.tar.gz` |
| Linux | ARM64 | `music-syncer_<version>_linux_arm64.tar.gz` |
| macOS | Intel | `music-syncer_<version>_darwin_amd64.tar.gz` |
| macOS | Apple Silicon | `music-syncer_<version>_darwin_arm64.tar.gz` |
| Windows | x86_64 | `music-syncer_<version>_windows_amd64.zip` |
| Windows | ARM64 | `music-syncer_<version>_windows_arm64.zip` |

В каждом архиве лежат `music-syncer` (в Windows — `music-syncer.exe`),
`README.md`, `LICENSE` и `config.example.yaml`. Рядом с архивами публикуется
`checksums.txt` с SHA-256.

### Linux/macOS

```bash
VERSION=0.1.0
OS=linux          # linux или darwin
ARCH=amd64        # amd64 или arm64
FILE="music-syncer_${VERSION}_${OS}_${ARCH}.tar.gz"

curl -LO "https://github.com/zxbass/music-syncer/releases/download/v${VERSION}/${FILE}"
tar -xzf "${FILE}"
mkdir -p "$HOME/.local/bin"
install -m 0755 music-syncer "$HOME/.local/bin/"
music-syncer version
```

Если `~/.local/bin` отсутствует в `PATH`, добавьте его в `~/.profile` или
`~/.zshrc`:

```bash
export PATH="$HOME/.local/bin:$PATH"
```

### Windows PowerShell

```powershell
$VERSION = "0.1.0"
$FILE = "music-syncer_${VERSION}_windows_amd64.zip"

Invoke-WebRequest `
  "https://github.com/zxbass/music-syncer/releases/download/v$VERSION/$FILE" `
  -OutFile $FILE

Expand-Archive $FILE -DestinationPath "music-syncer-$VERSION"
.\music-syncer-$VERSION\music-syncer.exe version
```

Для ARM64-версии Windows замените `amd64` на `arm64`.

Бинарники не подписаны: macOS Gatekeeper или Windows SmartScreen могут
потребовать подтверждения при первом запуске. Проверяйте SHA-256 по
`checksums.txt` перед запуском.

### Как выпускается релиз

Релиз запускается автоматически при пуше тега:

```bash
git tag -a v0.1.0 -m "music-syncer v0.1.0"
git push origin v0.1.0
```

GitHub Actions собирает Linux, macOS и Windows бинарники через GoReleaser,
формирует архивы, checksums и публикует GitHub Release. Повторный запуск
можно инициировать вручную через `workflow_dispatch` с указанием существующего
тега.

## Настройка Spotify

1. Откройте <https://developer.spotify.com/dashboard> и создайте приложение.
2. В настройках приложения добавьте redirect URI **ровно**:

   ```
   http://127.0.0.1:8085/callback
   ```

3. Скопируйте **Client ID** (Client Secret для PKCE не нужен).
4. Выполните:

   Linux/macOS:

   ```bash
   music-syncer auth spotify --client-id ВАШ_CLIENT_ID
   ```

   Windows PowerShell:

   ```powershell
   .\music-syncer.exe auth spotify --client-id "ВАШ_CLIENT_ID"
   ```

Браузер откроется автоматически. Если вы на удалённом сервере или браузер не
открывается:

```bash
music-syncer auth spotify --client-id ВАШ_CLIENT_ID --manual
```

```powershell
.\music-syncer.exe auth spotify --client-id "ВАШ_CLIENT_ID" --manual
```

Команда выведет URL. После подтверждения браузер попытается открыть
`http://127.0.0.1:8085/callback?...`; скопируйте **полный URL из адресной строки**
(даже если страница не загрузилась) и вставьте в терминал.

Запрашиваемые scopes:

```
user-read-private user-read-email
playlist-read-private playlist-read-collaborative
playlist-modify-private playlist-modify-public
user-library-read user-library-modify
user-follow-read user-follow-modify
user-read-recently-played
```

## Настройка Яндекс.Музыки

Основной способ — личный OAuth-токен. Получите его, открыв в браузере:

```
https://oauth.yandex.ru/authorize?response_type=token&client_id=23cabbbdc6cd418abb4b39c32c41195d
```

После подтверждения Яндекс перенаправит на `music.yandex.ru` с параметром
`access_token` во фрагменте URL. Скопируйте значение и передайте его:

```bash
music-syncer auth yandex --token y0_...
```

Windows PowerShell:

```powershell
.\music-syncer.exe auth yandex --token "y0_..."
```

Или сохраните в переменной окружения:

```bash
export YANDEX_MUSIC_TOKEN=y0_...
music-syncer auth yandex
```

```powershell
$env:YANDEX_MUSIC_TOKEN = "y0_..."
.\music-syncer.exe auth yandex
```

Чтобы переменная сохранилась между сеансами PowerShell:

```powershell
setx YANDEX_MUSIC_TOKEN "y0_..."
```

После `setx` откройте новый терминал.

Дополнительно поддерживается OAuth Device Flow. Для него нужен client secret
вашего OAuth-приложения (в репозитории он не хранится):

```bash
export YANDEX_MUSIC_CLIENT_ID=...
export YANDEX_MUSIC_CLIENT_SECRET=...
music-syncer auth yandex
```

```powershell
$env:YANDEX_MUSIC_CLIENT_ID = "..."
$env:YANDEX_MUSIC_CLIENT_SECRET = "..."
.\music-syncer.exe auth yandex
```

Команда покажет URL и короткий код. Подтвердите вход на любом устройстве —
токен сохранится автоматически.

## Быстрый старт

Linux/macOS:

```bash
# 1. Подключить аккаунты
music-syncer auth yandex --token y0_...
music-syncer auth spotify --client-id ВАШ_CLIENT_ID
music-syncer auth status

# 2. Посмотреть, что будет сделано
music-syncer transfer --from yandex --to spotify --dry-run

# 3. Перенести плейлисты, любимые треки и артистов (по умолчанию)
music-syncer transfer --from yandex --to spotify

# 4. Обратное направление
music-syncer transfer --from spotify --to yandex --only playlists,likes
```

Windows PowerShell:

```powershell
# 1. Подключить аккаунты
.\music-syncer.exe auth yandex --token "y0_..."
.\music-syncer.exe auth spotify --client-id "ВАШ_CLIENT_ID"
.\music-syncer.exe auth status

# 2. Посмотреть, что будет сделано
.\music-syncer.exe transfer --from yandex --to spotify --dry-run

# 3. Перенести плейлисты, любимые треки и артистов (по умолчанию)
.\music-syncer.exe transfer --from yandex --to spotify

# 4. Обратное направление
.\music-syncer.exe transfer --from spotify --to yandex --only playlists,likes
```

Если подключены оба аккаунта и не указаны `--from`/`--to`, по умолчанию
используется направление **Яндекс → Spotify**.

Перенос однонаправленный. Для двусторонней синхронизации запустите команду
дважды с разными `--from`/`--to` (лайки и артисты при этом только добавляются,
ничего не удаляется).

## Команды

В примерах ниже используется `music-syncer`; в Windows запускайте
`.\music-syncer.exe` (или `music-syncer.exe`, если каталог добавлен в `PATH`).

### `auth`

```bash
music-syncer auth yandex [--token TOKEN]
music-syncer auth spotify --client-id ID [--manual] [--no-browser] [--redirect-uri URI]
music-syncer auth status
music-syncer auth logout yandex|spotify
```

### `playlists`

Список плейлистов провайдера. Флаги `generated`, `read-only`, `liked`
показывают, что будет пропущено при обычном переносе.

```bash
music-syncer playlists --provider yandex
music-syncer playlists --provider spotify --json
```

### `transfer`

```bash
music-syncer transfer [--from yandex] [--to spotify] [флаги]
```

| Флаг | Описание |
|---|---|
| `--from`, `--to` | `yandex` или `spotify`; второй провайдер выводится автоматически |
| `--only` | `playlists,likes,artists` или `all` (по умолчанию `playlists,likes,artists`) |
| `--playlist` | фильтр по названию, ID, kind или подстроке; можно повторять |
| `--all-playlists` | включить сгенерированные/алгоритмические плейлисты |
| `--prefix` | префикс для названий создаваемых плейлистов, например `"[YM] "` |
| `--mirror` | удалять из целевого плейлиста треки, которых нет в источнике |
| `--dry-run` | только показать план, ничего не записывать |
| `--limit N` | ограничить число треков в каждой коллекции (удобно для теста) |
| `--threshold` | порог сопоставления 0.0–1.0 (по умолчанию 0.72) |
| `--report PATH` | сохранить полный отчёт в JSON |

Особенности:

- плейлисты переносятся **аддитивно**: существующие треки не удаляются;
- повторный запуск идемпотентен: связи и совпадения кэшируются в `state.json`,
  созданные плейлисты находятся по сохранённой связи или по имени;
- `--mirror` удаляет лишнее только если **все** исходные треки сопоставлены
  (и если источник не был урезан через `--limit`);
- импортированные плейлисты создаются приватными;
- при ошибках API команда завершается с кодом `1`, частично успешный результат
  остаётся в отчёте и в состоянии.

### `export`

Локальный JSON-бэкап:

```bash
music-syncer export --provider yandex --output yandex-backup
music-syncer export --provider spotify --only likes,artists
```

Файлы:

```
playlists.json       массив {playlist, tracks}
liked-tracks.json
artists.json
summary.json
```

Плейлисты, доступные только для чтения, экспортируются без списка треков
(в `summary.json` будет примечание).

### `match`

Диагностика сопоставления без записи в аккаунты:

```bash
music-syncer match --to spotify \
  --title "Группа крови" --artist "Кино" --duration 283

music-syncer match --to yandex \
  --title "Blinding Lights" --artist "The Weeknd" --json
```

Вывод показывает score, вклад названия/артиста/длительности и какой кандидат
был бы выбран.

### `cache`

```bash
music-syncer cache stats
music-syncer cache clear
```

`cache clear` удаляет только локальный кэш совпадений и связей плейлистов.
Музыкальные аккаунты не изменяются. Следующий запуск будет искать всё заново.

## Как работает сопоставление

Для каждого исходного трека:

1. проверяется локальный кэш (`state.json`);
2. трек ищется среди уже существующих треков целевой коллекции по
   нормализованному названию (учитывается транслитерация);
3. если ISRC известен, выполняется точный поиск по ISRC;
4. иначе — поиск в каталоге target-сервиса по названию и основному артисту;
5. кандидаты ранжируются:
   - 55% — схожесть названия,
   - 33% — схожесть артиста,
   - 12% — близость длительности,
   - небольшой бонус за совпадение альбома,
   - точное совпадение ISRC сразу принимается;
6. версии `live`, `remix`, `acoustic`, `instrumental`, `karaoke`, `demo`,
   `sped up`, `slowed`, `8D` считаются разными записями и **не** принимаются;
   `remastered`, `deluxe`, `explicit`, `mono/stereo` игнорируются;
7. названия с разными номерами частей («Part 2» / «Part 3») не считаются
   одним треком;
8. итоговый порог по умолчанию **0.72**, плюс минимальные пороги для названия
   и артиста.

Всё, что не прошло порог, попадает в отчёт с лучшим кандидатом и причиной.
Порог можно временно изменить через `--threshold` или навсегда в
`config.yaml` (`match.threshold`).

Ограничения подхода: классическая музыка, инструменталы, каверы, сильно
переименованные издания и региональные различия каталогов иногда требуют
ручного вмешательства. Такие треки лучше искать через `export` + отчёт.

## Конфигурация и состояние

Пути по умолчанию:

| ОС | Конфиг | Состояние |
|---|---|---|
| Linux | `~/.config/music-syncer/config.yaml` | `~/.config/music-syncer/state.json` |
| macOS | `~/Library/Application Support/music-syncer/config.yaml` | `~/Library/Application Support/music-syncer/state.json` |
| Windows | `%AppData%\music-syncer\config.yaml` | `%AppData%\music-syncer\state.json` |

На Windows это обычно:

```
C:\Users\<имя>\AppData\Roaming\music-syncer\config.yaml
C:\Users\<имя>\AppData\Roaming\music-syncer\state.json
```

- `--config PATH` — другой файл конфигурации;
- `MUSIC_SYNCER_CONFIG` — то же через переменную окружения;
- `MUSIC_SYNCER_STATE` — отдельный путь к `state.json`;
- `YANDEX_MUSIC_TOKEN` — переопределяет токен Яндекса;
- `YANDEX_MUSIC_CLIENT_ID` и `YANDEX_MUSIC_CLIENT_SECRET` — необязательные
  параметры для OAuth Device Flow Яндекса;
- `SPOTIFY_CLIENT_ID` — client ID по умолчанию.

Windows PowerShell:

```powershell
.\music-syncer.exe --config "D:\music-syncer\config.yaml" auth status
$env:MUSIC_SYNCER_CONFIG = "D:\music-syncer\config.yaml"
$env:MUSIC_SYNCER_STATE = "D:\music-syncer\state.json"
```

Пример конфигурации: [`config.example.yaml`](config.example.yaml).

Файлы содержат OAuth-токены. Не коммитьте их и не пересылайте. На Linux и
macOS приложение создаёт их с правами `0600`, а `auth status` предупредит, если
права ослаблены. На Windows POSIX-права не применяются — доступ определяется
ACL профиля пользователя, поэтому не размещайте конфиг в общедоступных или
синхронизируемых каталогах.

## Отчёты

```bash
music-syncer transfer --from yandex --to spotify --report report.json
```

В отчёте:

- статистика по каждому плейлисту (source/added/existing/unmatched/failed);
- удалённые треки при `--mirror`;
- несопоставленные треки и лучшие кандидаты с разбивкой score;
- несопоставленные артисты;
- ошибки API;
- заметки (устаревшая ссылка, переиспользован плейлист по имени и т.п.).

## Архитектура

```
cmd/music-syncer/        точка входа
internal/cli/            cobra-команды, вывод, отчёты
internal/domain/         общие модели и интерфейс Provider
internal/yandex/         клиент Яндекс.Музыки, device flow, адаптер
internal/spotify/        клиент Spotify, PKCE, адаптер
internal/matcher/        нормализация, транслитерация, скоринг
internal/syncer/         движок переноса (плейлисты, лайки, артисты)
internal/store/          локальное состояние и кэш
internal/httpx/          rate limit, retry, обработка ошибок
internal/config/         конфигурация
```

Ключевая идея: `syncer.Engine` не знает про HTTP и конкретные сервисы. Он
работает через `domain.Provider`, поэтому новый провайдер — это реализация
интерфейса и регистрация в CLI.

## Разработка

Linux/macOS:

```bash
make test          # go test ./...
make test-race     # go test -race ./...
make vet           # go vet ./...
make build         # бинарник с версией из git
make run           # go run ./cmd/music-syncer --help
```

Windows PowerShell (Makefile не обязателен):

```powershell
go test ./...
go test -race ./...
go vet ./...
go build -trimpath -ldflags "-s -w" -o music-syncer.exe .\cmd\music-syncer
go run .\cmd\music-syncer --help
```

Тесты не ходят в реальные API: HTTP-клиенты проверяются через `httptest`,
движок — через фейковые провайдеры.

## Roadmap

- ручные overrides для несопоставленных треков (`match set`);
- слияние/сравнение двух библиотек без записи (`diff`);
- инкрементальные снапшоты и восстановление из экспорта;
- поддержка Apple Music / Deezer через тот же интерфейс;
- TUI-режим просмотра отчёта.

## Правовая информация

Проект не связан с Яндексом и Spotify. Яндекс.Музыка используется через
неофициальные эндпоинты; это может нарушать условия сервиса. Spotify Web API
используется в рамках официальных правил и scopes. Используйте инструмент для
собственных аккаунтов и на свой риск.

## Лицензия

MIT — см. [LICENSE](LICENSE).
