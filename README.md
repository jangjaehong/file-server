# File Server — 프로그램 배포 & 자동 업데이트 서버

웹에서 프로그램을 **업로드(관리자)** / **다운로드(누구나)** 하고, 각 프로그램이 스스로
**자동 업데이트** 할 수 있도록 배포하는 서버입니다.

- Go 1.27 단일 실행 파일 (CGO 불필요, Windows/Linux 모두 빌드 가능) + 내장 SQLite + 내장 웹 UI
- 인터넷이 없는 폐쇄망(SCADA/공장망 등)에서도 외부 CDN 없이 동작
- 프로그램별 설명, 버전별 패치노트(Markdown), 플랫폼별 파일, SHA-256 체크섬, 다운로드 수
- 자동 업데이트 API + Go 클라이언트 라이브러리(`pkg/updater`) + CLI(`fsctl`)
- 기존 IIS 방식(`/<프로그램>/latest.json`)도 그대로 제공 → **One TaskBar, ICS-SCM은 클라이언트 수정 없이 연동**
  ([docs/배포-운영.md](docs/배포-운영.md))

---

## 1. 빠른 시작

### Docker (권장)

> 운영 서버 배포는 **PC에서 이미지를 빌드해 서버로 옮기는 방식**을 씁니다:
> `.\tools\docker\Deploy-Image.ps1 -Server deploy@211.231.121.41` — 자세한 절차는 [docs/배포-운영.md](docs/배포-운영.md) 2절.
> 아래는 서버에서 소스를 받아 직접 빌드하는 방법입니다.

```bash
cp .env.example .env          # FS_ADMIN_PASSWORD, FS_BASE_URL 수정
docker compose up -d --build
# http://서버:8080          공개 다운로드 페이지
# http://서버:8080/#/admin  관리자
```

- 이미지: `golang:1.27-alpine` 으로 빌드 → `distroless/static` 위에서 실행 (약 34MB, 셸 없음, non-root uid 65532)
- 데이터(DB + 업로드 파일)는 볼륨 `fs-data` → 컨테이너 `/data` 에 저장. 이미지를 바꿔도 유지됩니다.
- `HEALTHCHECK` 내장 (`/healthz`), `restart: unless-stopped`
- 관리자 비밀번호 변경: `.env` 의 `FS_ADMIN_PASSWORD` 수정 후 `docker compose up -d` (재시작 시 반영)
- 로그인이 안 되면 `docker compose logs file-server | grep -i admin` 으로 계정 적용 결과를 확인하세요.
- 백업: `docker run --rm -v file-server_fs-data:/data -v $PWD:/backup alpine tar czf /backup/fs-backup.tgz -C /data .`
  (SQLite WAL 사용 중이므로 일관된 백업을 원하면 잠깐 `docker compose stop` 후 실행)
- 업데이트: `git pull && docker compose up -d --build`
- `FS_BASE_URL` 은 클라이언트가 접속하는 주소(예: `http://192.168.0.10:8080`)로 꼭 지정하세요.
  업데이트 API 응답의 다운로드 URL 이 이 주소로 만들어집니다.
- 폐쇄망 서버라면 인터넷 되는 PC에서 `docker build -t file-server:1.0 .` →
  `docker save file-server:1.0 | gzip > file-server.tar.gz` → 반입 후 `docker load < file-server.tar.gz`

### 직접 실행

```bash
go build -o bin/ ./cmd/...                    # 또는 make build / make release(크로스 컴파일)

FS_ADMIN_PASSWORD='change-me-please' ./bin/server -addr :8080 -data ./data
# 브라우저: http://localhost:8080          (공개 다운로드 페이지)
#           http://localhost:8080/#/admin   (관리자: admin / change-me-please)
```

관리자 계정 규칙:
- `FS_ADMIN_PASSWORD` 가 설정되어 있으면 **시작할 때마다** `FS_ADMIN_USER`(기본 `admin`) 계정을 그 비밀번호로 맞춥니다.
  `.env` 를 고치고 재시작하면 바로 반영됩니다. (이 경우 `set-admin` 으로 바꾼 비밀번호는 재시작 시 덮어써집니다.)
- 설정하지 않았고 계정이 없으면 랜덤 비밀번호를 생성해 **로그에 한 번만** 출력합니다.
- 비밀번호는 최소 4자입니다. 8자 미만이면 경고 로그를 남깁니다. 잘못된 값이어도 서버는 멈추지 않고 로그에 이유를 남깁니다.

```bash
./bin/server set-admin -data ./data -user admin -password 'new-password'   # 비밀번호 변경/계정 추가
```

| 플래그 / 환경변수 | 기본값 | 설명 |
|---|---|---|
| `-addr` / `FS_ADDR` | `:8080` | 리슨 주소 |
| `-data` / `FS_DATA` | `./data` | DB(`fileserver.db`)와 파일(`files/`) 저장 위치 |
| `-base-url` / `FS_BASE_URL` | 요청 Host | 업데이트 응답에 들어갈 외부 URL (리버스 프록시 뒤에서 설정 권장) |
| `-max-upload-mb` | `4096` | 파일 1개 최대 크기 |
| `-tls-cert`, `-tls-key` | – | 직접 HTTPS 서비스 |
| `-secure-cookies` / `FS_SECURE_COOKIES=1` | – | HTTPS 프록시 뒤에서 사용 시 |

백업은 `data/` 디렉터리 하나만 복사하면 됩니다.

---

## 2. 버전 관리 전략 (업로드하는 사람의 고민에 대한 답)

> "A 프로그램을 만들고 계속 업데이트할 때, 버전을 어떻게 관리하고 다운로드로 제공해야 하나?"

이 서버는 아래 **7가지 규칙**으로 그 문제를 해결하도록 설계되어 있습니다.

### ① 프로그램(Program)과 릴리스(Release)를 분리한다
```
Program  scada-viewer            ← 고정 ID(slug), 이름, 설명 (한 번 등록)
 ├─ Release 1.0.0  stable        ← 패치노트 + 파일들
 │    ├─ windows-amd64/ScadaViewer.exe
 │    └─ linux-amd64/scada-viewer
 ├─ Release 1.1.0  stable  [필수]
 └─ Release 1.2.0-beta.1  beta
```
업로드는 항상 "프로그램 A 의 **새 버전**을 추가"하는 행위입니다. 기존 파일을 덮어쓰지 않습니다.

### ② 버전 번호는 SemVer(`MAJOR.MINOR.PATCH`)로 강제한다
- `1.4.2` → 버그 수정은 PATCH, 기능 추가는 MINOR, 호환성이 깨지면 MAJOR
- Windows 파일 버전 형식 `1.0.0.57`(끝자리 = CI 빌드 번호)도 지원합니다. `1.0.0.9 < 1.0.0.10` 으로 숫자 비교합니다.
- 베타/시험판은 `2.0.0-beta.1`, `2.0.0-rc.1` 처럼 pre-release 표기
- "최신 버전"은 **업로드 시각이 아니라 버전 번호**로 계산합니다 (`1.10.0 > 1.9.0` 을 올바르게 비교).
  그래서 1.x 유지보수 패치를 2.x 이후에 올려도 최신이 뒤바뀌지 않습니다.

### ③ 한 번 배포된 버전은 불변(immutable)
- 같은 버전 번호로 다시 올릴 수 없습니다 (`409 Conflict`).
- 배포된 릴리스의 파일은 추가/삭제/교체 불가. 문제가 있으면 **`1.4.3` 처럼 새 버전**을 올립니다.
- 이유: 사용자 PC마다 "1.4.2" 가 서로 다른 파일이면 장애 분석·재현이 불가능해집니다.
  모든 파일에는 SHA-256 이 기록되어 클라이언트가 무결성을 검증합니다.

### ④ 초안 → 배포 → 회수 의 라이프사이클
| 상태 | 공개 페이지 | 자동 업데이트 | 파일 변경 | 삭제 |
|---|---|---|---|---|
| **초안 (draft)** | 숨김 | 제외 | 가능 | 가능 |
| **배포중 (published)** | 표시 | 대상 | 불가 | 불가 (회수만) |
| **회수됨 (yanked)** | 숨김 | 제외 + 해당 버전 사용자에게 **강제 업데이트** | 불가 | 불가 |

초안 상태에서 Windows/Linux 파일과 패치노트를 모두 준비한 뒤 한 번에 [배포] 합니다.
치명적 버그가 발견되면 [회수] 하면, 그 버전을 쓰는 클라이언트는 다음 업데이트 확인 때 안전한 버전으로 이동합니다.

### ⑤ 채널 (stable / beta)
- 일반 사용자는 `stable` 만 받습니다.
- 시험 사용자는 `channel=beta` 로 확인하면 beta + stable 중 최신을 받습니다.

### ⑥ 필수 업데이트 플래그
릴리스에 "필수 업데이트"를 체크하면, 그보다 낮은 버전 사용자가 업데이트 확인 시
`mandatory: true` 를 받습니다. 1.0.0 사용자가 1.3.0 으로 갈 때 중간의 1.1.0 이 필수였다면
`mandatory` 가 true 가 됩니다(중간 버전을 건너뛰어도 필수 여부를 놓치지 않음).
또한 건너뛴 모든 버전의 패치노트(`changes`)를 한 번에 돌려줘 "무엇이 바뀌었나"를 보여줄 수 있습니다.

### ⑦ 고정 URL 과 저장 구조
```
/download/scada-viewer/latest/windows-amd64          ← 항상 최신 stable (매뉴얼·바로가기에 사용)
/download/scada-viewer/latest/windows-amd64?channel=beta
/download/scada-viewer/1.1.0/windows-amd64           ← 특정 버전 고정(롤백용)
/files/{id}                                          ← 자동 업데이트가 사용하는 파일 URL

data/files/scada-viewer/1.1.0/windows-amd64/ScadaViewer.exe   ← 실제 저장 경로(버전별 디렉터리)
```

### 권장 업로드 워크플로
1. **수동**: 관리자 페이지 → 프로그램 선택 → *새 버전 만들기*(다음 PATCH 번호가 자동 제안됨)
   → 플랫폼 선택 후 파일 업로드 → 패치노트 작성 → [배포]
2. **자동(CI/빌드 스크립트)**: 관리자 페이지 → *API 토큰* 생성 후
   ```bash
   fsctl publish -server http://files.local:8080 -token fst_xxx \
     -program scada-viewer -name "SCADA Viewer" \
     -version 1.4.0 -notes-file CHANGELOG/1.4.0.md \
     -file windows-amd64=dist/ScadaViewer.exe -file linux-amd64=dist/scada-viewer
   # -draft : 업로드만 하고 배포는 웹에서 검토 후
   # -mandatory : 필수 업데이트
   # -channel beta : 베타 채널
   ```
   - 프로그램이 없으면 자동 생성되고, 같은 버전·같은 파일로 다시 실행하면 "이미 배포됨"으로 성공합니다(CI 재실행 안전).
   - `-notes-from-git v1.3.0` 으로 커밋 제목에서 패치노트를 만들 수 있습니다.
   - 사내 PC에서 직접 빌드·전송하는 절차와 스크립트 레퍼런스: [docs/배포-운영.md](docs/배포-운영.md)
   소스의 git 태그(`v1.4.0`)와 버전을 일치시키고, 빌드 시 `-ldflags "-X main.Version=1.4.0"` 로
   프로그램 안에 버전을 박아 두면 업데이트 확인에 그대로 사용할 수 있습니다.

---

## 3. 자동 업데이트 연동

### API
```
GET /api/v1/update?program=scada-viewer&version=1.0.0&platform=windows-amd64[&channel=beta]
```
```json
{
  "program": "scada-viewer",
  "current_version": "1.0.0",
  "update_available": true,
  "mandatory": true,
  "current_yanked": false,
  "latest": {
    "version": "1.1.0", "channel": "stable", "platform": "windows-amd64",
    "filename": "ScadaViewer.exe", "size": 1048576,
    "sha256": "81db67b6…", "url": "http://files.local:8080/files/3"
  },
  "changes": [ { "version": "1.1.0", "mandatory": true, "notes": "### 수정\n- …" } ]
}
```
- `platform` 은 `windows-amd64`, `linux-arm64` 등 자유 문자열. 해당 플랫폼 파일이 없으면 `any` 파일을 사용합니다.
- 다운로드는 HTTP Range 를 지원하므로 끊긴 다운로드를 이어받을 수 있습니다.

### Go 프로그램에 내장
```go
import "github.com/jangjaehong/file-server/pkg/updater"

var Version = "1.0.0" // go build -ldflags "-X main.Version=1.1.0"

func checkUpdate(ctx context.Context) {
    updater.CleanupOld()                                  // 지난 업데이트의 .old 정리
    u := updater.New("http://files.local:8080", "scada-viewer")
    info, err := u.Check(ctx, Version)
    if err != nil || !info.UpdateAvailable { return }
    // info.Mandatory 이면 사용자 확인 없이 진행, 아니면 패치노트(info.Changes)를 보여주고 확인
    path, err := u.Download(ctx, info, "", nil)          // SHA-256 검증 + 이어받기
    if err != nil { return }
    if err := updater.ReplaceExecutable(path); err == nil {
        // 프로그램 재시작
    }
}
```

### 다른 언어 (C#, JavaScript 등)
HTTP GET 한 번으로 확인 → `latest.url` 다운로드 → `sha256` 비교 → 설치/교체 순서로 구현하면 됩니다.
```js
// Node.js / Electron 예시
const q = new URLSearchParams({ program: 'scada-viewer', version: APP_VERSION, platform: 'windows-amd64' });
const info = await (await fetch(`http://files.local:8080/api/v1/update?${q}`)).json();
if (info.update_available) {
  const buf = Buffer.from(await (await fetch(info.latest.url)).arrayBuffer());
  const hash = require('crypto').createHash('sha256').update(buf).digest('hex');
  if (hash !== info.latest.sha256) throw new Error('checksum mismatch');
  // 파일 저장 후 설치 프로그램 실행 또는 교체
}
```

### CLI (`fsctl`) — 별도 런처/스케줄러에서 사용
```bash
fsctl check  -server http://files.local:8080 -program scada-viewer -version 1.0.0
fsctl update -server http://files.local:8080 -program scada-viewer -version 1.0.0 \
             -platform windows-amd64 -target "C:\Apps\ScadaViewer\ScadaViewer.exe"
fsctl download -server http://files.local:8080 -program scada-viewer -o .   # latest
```
`update -target` 은 기존 파일을 `*.old` 로 백업 후 교체합니다(실패 시 원복).
실행 중인 서비스라면 서비스 중지 → `fsctl update` → 서비스 시작 순서의 스크립트로 감싸세요.

---

## 4. 보안

- 업로드/수정/삭제는 관리자 로그인(세션 쿠키: HttpOnly, SameSite=Strict) 또는 API 토큰(Bearer)만 가능
- 비밀번호 bcrypt, 세션·토큰은 SHA-256 해시로만 DB 저장, 토큰은 생성 시 1회만 표시
- 로그인 5회 실패 시 IP 단위 지연, 쿠키 인증 변경 요청엔 `X-Requested-With` 헤더 필수(CSRF 방어)
- 파일명 정규화(경로 조작 차단), 업로드는 스트리밍 저장(대용량도 메모리 사용 일정)
- 운영 시 HTTPS 권장 (`-tls-cert/-tls-key` 또는 nginx 등 리버스 프록시 + `-base-url`, `-secure-cookies`)

## 5. 관리 API 요약

| Method | Path | 설명 |
|---|---|---|
| POST | `/api/v1/auth/login` | `{username,password}` |
| GET/POST | `/api/v1/admin/programs` | 프로그램 목록/등록 `{slug,name,summary,description}` |
| PUT/DELETE | `/api/v1/admin/programs/{slug}` | 수정/삭제 |
| GET/POST | `/api/v1/admin/programs/{slug}/releases` | 버전 목록/초안 생성 `{version,channel,notes,mandatory}` |
| PUT/DELETE | `…/releases/{version}` | 패치노트·채널·필수 수정 / 초안 삭제 |
| POST | `…/releases/{version}/assets` | multipart: `platform`, `file`(여러 개) |
| POST | `…/releases/{version}/publish` \| `yank` \| `unpublish` | 상태 변경 |
| DELETE | `/api/v1/admin/assets/{id}` | 초안의 파일 삭제 |
| GET/POST | `/api/v1/admin/programs/{slug}/files` | 고정 파일(defaults.json 등) 목록/업로드(같은 이름은 교체) |
| DELETE | `/api/v1/admin/programs/{slug}/files/{name}` | 고정 파일 삭제 |
| GET/POST/DELETE | `/api/v1/admin/tokens[/{id}]` | API 토큰 |

정적 업데이트 주소(기존 IIS 호환): `GET /{slug}/latest.json`, `GET /{slug}/{파일}` — [docs/배포-운영.md](docs/배포-운영.md)

공개 API: `GET /api/v1/programs`, `/api/v1/programs/{slug}`, `/api/v1/programs/{slug}/releases[/{version}]`, `/api/v1/update`

## 6. 구조

```
cmd/server      서버 실행 파일 (+ set-admin, healthcheck)
cmd/fsctl       업로드(publish)·업데이트(check/update/download) CLI
internal/server HTTP 핸들러, 인증, 업로드/다운로드, 업데이트 API
internal/store  SQLite 저장소 (programs / releases / assets / admins / sessions / api_tokens)
internal/semver Semantic Versioning 파싱·비교
pkg/updater     프로그램에 내장하는 자동 업데이트 클라이언트 라이브러리
pkg/publish     배포 로직 (fsctl publish; Publish-FileServer.ps1과 동일한 규칙)
tools/Publish-FileServer.ps1  Windows 빌드 PC용 배포 스크립트 (기존 Publish-Internal.ps1 대체)
tools/docker/Deploy-Image.ps1 PC에서 이미지 빌드 → 서버로 복사 → 로드·재시작
docs/배포-운영.md             서버 운영자용 배포·운영 가이드 (One TaskBar · ICS-SCM)
web/static      내장 웹 UI (Vanilla JS, 외부 의존성 없음)
Dockerfile, docker-compose.yml, .env.example   Docker 배포
```
