# One TaskBar · ICS-SCM 배포/자동 업데이트 (기존 IIS "Auto Update Server" 대체)

이 파일서버는 사내 Windows 프로그램 두 개의 배포와 자동 업데이트를 맡습니다.

| 프로그램 | 저장소 | slug (주소의 폴더 이름) | 클라이언트 기본 업데이트 주소 |
|---|---|---|---|
| One TaskBar | `jangjaehong/OneTaskBarAtWindows` (C#) | `onetaskbar` | `http://211.231.121.41:9100/onetaskbar/` |
| ICS-SCM (Git 형상관리) | `jangjaehong/ics-scm` (Go) | `ics-scm` | `http://211.231.121.41:9100/ics-scm/` |

## 1. 동작 방식 — 클라이언트 수정 없음

두 프로그램은 IIS 폴더에 놓인 정적 파일로 업데이트합니다.

```
<업데이트 주소>/latest.json     {"version":"1.0.0.57","file":"OneTaskBar-Setup-1.0.0.57.exe","sha256":"…","notes":"…"}
<업데이트 주소>/<file>          latest.json이 가리키는 exe
<업데이트 주소>/defaults.json   (One TaskBar) 회사 기본 설정
```

파일서버는 **모든 프로그램에 대해 같은 주소를 그대로 제공**합니다.

| 요청 | 파일서버의 응답 |
|---|---|
| `GET /onetaskbar/latest.json` | 배포중인 **stable** 버전 중 가장 높은 **숫자 버전**(`1.0.0.57` 형식)의 `.exe`로 생성 |
| `GET /onetaskbar/OneTaskBar-Setup-1.0.0.57.exe` | 해당 버전 파일 (Range 이어받기, SHA-256 헤더) |
| `GET /onetaskbar/defaults.json` | 관리자 화면 "고정 파일"에 올린 파일 |

- 업로드 파일 이름에 버전이 없으면 자동으로 붙여서 제공합니다: `OneTaskBar-Setup.exe` → `OneTaskBar-Setup-1.0.0.57.exe`
- `2.0.0-beta.1`처럼 문자가 들어간 버전은 .NET `Version`이 읽지 못하므로 `latest.json`에 나오지 않습니다.
  시험 배포는 `channel=beta` + 숫자 버전으로 올리고, 시험 PC의 업데이트 주소를 `…/onetaskbar/latest.json?channel=beta`로 지정하세요.
- **회수(yank)** 하면 즉시 `latest.json`이 이전 버전으로 돌아갑니다. 단, 현재 클라이언트는 "더 높은 버전"만 설치하므로
  이미 회수된 버전을 설치한 PC는 자동으로 내려가지 않습니다. 그 PC에는 더 높은 번호의 수정 버전을 배포하세요.
- `latest.json`에는 `mandatory`, `size`, `published_at` 필드가 추가로 들어갑니다. 기존 클라이언트는 이 필드를 무시합니다.

ICS-SCM의 실제 업데이트 코드(`internal/update/update.go`)로 latest.json 읽기 → 버전 비교 → 다운로드 → SHA-256 검증까지
파일서버에 대해 동작하는 것을 확인했습니다. One TaskBar(`src/AppUpdate.cs`)도 같은 형식과 같은 상대 경로 규칙을 씁니다.

## 2. 서버 전환 방법

### A안 (권장) — 같은 서버에서 IIS 사이트를 교체
클라이언트 설정을 하나도 바꾸지 않아도 됩니다.

1. 파일서버를 띄우고 (아래 3번) 두 프로그램을 등록한 뒤 **현재 배포 중인 버전을 같은 버전 번호로 업로드**합니다.
2. `http://<새 서버>:<포트>/onetaskbar/latest.json`이 IIS의 것과 같은 version/sha256인지 확인합니다.
3. IIS의 "Auto Update Server" 사이트(9100)를 중지하고 파일서버를 **9100 포트**로 실행합니다 (`.env`에 `FS_PORT=9100`).
4. 문제가 생기면 파일서버를 멈추고 IIS 사이트를 다시 시작하면 원상복구됩니다(IIS 폴더는 그대로 두세요).

### B안 — 다른 서버(Docker 호스트)에서 운영
- **IIS를 프록시로 사용**: 9100 사이트에 URL Rewrite + ARR로 `http://<docker 호스트>:8080/{R:0}` 역방향 프록시를 설정하면
  클라이언트 변경이 필요 없습니다.
- **클라이언트 주소 변경**: 각 PC의 설정 → 업데이트 서버를 새 주소로 바꾸거나, 두 저장소의 기본 주소 상수
  (`src/AppUpdate.cs`의 `DefaultServer`, `internal/update/update.go`의 `DefaultServer`)를 바꾼 버전을
  **마지막으로 기존 IIS에 배포**하면 이후부터 새 서버를 봅니다.

## 3. 서버 준비 (Docker)

```bash
cp .env.example .env     # FS_PORT=9100, FS_ADMIN_PASSWORD 설정
docker compose up -d --build
```
Windows Server에서 Docker 없이 실행하려면 `make release`로 만든 `bin/windows-amd64/server.exe`를
`server.exe -addr :9100 -data D:\services\file-server` 처럼 실행하고, NSSM이나 작업 스케줄러로 서비스처럼 등록하세요.

관리자 페이지(`/#/admin`)에서:
1. 프로그램 등록: slug `onetaskbar` / 이름 `One TaskBar`, slug `ics-scm` / 이름 `ICS-SCM`
   (slug는 업데이트 주소의 폴더 이름과 **정확히 같아야** 합니다)
2. **API 토큰** 생성 (빌드 PC에서 업로드할 때 사용)

## 4. 배포 도구 — `tools/Publish-FileServer.ps1` / `fsctl publish`

두 도구는 같은 동작을 합니다. 이 저장소의 `tools/Publish-FileServer.ps1`이 원본이며,
`ics-scm`, `OneTaskBarAtWindows` 저장소는 이 파일을 `tools/`에 복사해 씁니다.

**동작 (여러 번 실행해도 안전)**
1. 프로그램이 없으면 **자동 생성** (최초 배포). 이름은 `-Name`, 없으면 slug.
2. 버전이 없으면 생성 → 파일 업로드 → 배포.
3. 같은 버전이 **초안**이면 이어서 진행 (같은 플랫폼의 다른 파일은 교체).
4. 같은 버전이 이미 **배포됨**: SHA-256이 같으면 "이미 배포됨"으로 **성공(exit 0)**, 다르면 **오류(exit 1)**.
   배포된 버전은 바꿀 수 없으니 버전 번호를 올려 다시 빌드하세요.
5. 회수된 버전이면 오류.

**버전 결정 순서:** `-Version` → 파일 옆 `version.txt` → exe의 파일 버전(Windows에서만)

| PowerShell | fsctl | 설명 |
|---|---|---|
| `-Server` | `-server` | 기본값 `$env:FS_SERVER` |
| `-Token` | `-token` | 기본값 `$env:FS_TOKEN` |
| `-Program` (필수) | `-program` | slug: `onetaskbar`, `ics-scm` |
| `-Name` | `-name` | 최초 생성 시 표시 이름 (기본: slug) |
| `-File` + `-Platform` (기본 `windows-amd64`) | `-file windows-amd64=경로` | 배포할 exe |
| `-Version` | `-version` | 예: `1.0.0.57` |
| `-Channel stable\|beta` | `-channel` | 기본 stable |
| `-Notes` / `-NotesFile` | `-notes` / `-notes-file` | 패치노트 (Markdown) |
| `-NotesFromGit <ref>` 또는 `<a>..<b>` | `-notes-from-git` | 커밋 제목으로 패치노트 생성. `-Notes`/`-NotesFile`이 있으면 무시 |
| `-Mandatory` | `-mandatory` | 필수 업데이트 |
| `-Draft` | `-draft` | 업로드만, 배포는 관리자 화면에서 |
| `-FixedFile defaults.json` | `-fixed defaults.json` | 버전과 무관한 고정 파일 (교체) |
| `-SkipIfUnconfigured` | `-skip-if-unconfigured` | 서버/토큰이 비어 있으면 아무것도 안 하고 성공 (시크릿 없는 CI) |

PowerShell 스크립트는 GitHub Actions에서 쓸 수 있도록 `$GITHUB_OUTPUT`에
`version`, `status`(`published` / `already-published` / `draft`), `latest_json`을 기록합니다.

```powershell
# 빌드 PC에서 수동 배포
$env:FS_SERVER='http://211.231.121.41:9100'; $env:FS_TOKEN='fst_...'
.\tools\Publish-FileServer.ps1 -Program onetaskbar -Name "One TaskBar" -File release\OneTaskBar-Setup.exe -Notes "알림 개선"
.\tools\Publish-FileServer.ps1 -Program ics-scm -Name "ICS-SCM" -File dist\ics-scm.exe -NotesFromGit v1.0.0
.\tools\Publish-FileServer.ps1 -Program onetaskbar -FixedFile .\defaults.json
```

## 5. CI 자동 배포 (빌드 → 파일서버 → 자동 업데이트)

```
개발자 push (main)
  └─ GitHub Actions: 빌드 (버전 = X.Y.Z.<커밋 수>)
       └─ Publish-FileServer.ps1 / fsctl publish   ← FS_SERVER, FS_TOKEN 시크릿
            ├─ (최초 1회) 프로그램 자동 생성
            └─ 새 버전 생성 → exe 업로드 → 배포
                 └─ /<slug>/latest.json 갱신
                      └─ 각 PC: 업데이트 확인 → 다운로드 → SHA-256 검증 → 설치
```

### 5-1. 준비 (한 번)
1. 파일서버 관리자 페이지 → **API 토큰** → 이름 `github-ci` 등으로 생성, 표시된 값을 복사합니다 (한 번만 표시).
2. 각 저장소(ics-scm, OneTaskBarAtWindows) → GitHub **Settings → Secrets and variables → Actions**:
   - `FS_SERVER` = `http://211.231.121.41:9100`
   - `FS_TOKEN` = 위에서 복사한 토큰
3. 시크릿이 없으면 배포 단계는 **건너뛰고** 빌드는 성공합니다 (`-SkipIfUnconfigured`). 포크나 시크릿 설정 전에도 CI가 깨지지 않습니다.

### 5-2. 워크플로 예시 (배포 단계)
```yaml
      - name: Publish to file server
        if: github.ref == 'refs/heads/main'
        shell: pwsh
        env:
          FS_SERVER: ${{ secrets.FS_SERVER }}
          FS_TOKEN: ${{ secrets.FS_TOKEN }}
        run: ./tools/Publish-FileServer.ps1 -Program ics-scm -Name "ICS-SCM" -File dist/ics-scm.exe -SkipIfUnconfigured -NotesFromGit "HEAD~20"
```
- `-NotesFromGit`과 커밋 수 기반 버전은 git 기록이 필요하므로 `actions/checkout`에 `fetch-depth: 0`을 지정하세요.
- 같은 커밋으로 워크플로를 다시 실행해도 같은 버전과 같은 파일이므로 "이미 배포됨"으로 성공합니다.
- 다시 빌드했는데 결과물이 달라졌다면(재현 불가능한 빌드) 같은 버전에 다른 파일이 되어 실패합니다.
  이 경우 커밋을 하나 더 쌓아 버전을 올리세요.
- Linux 러너에서 PowerShell 대신 Go 도구를 쓰려면:
  `go run github.com/jangjaehong/file-server/cmd/fsctl@latest publish -program ics-scm -file windows-amd64=dist/ics-scm.exe -skip-if-unconfigured`

### 5-3. GitHub 클라우드 러너에서 사내 서버에 접근이 안 될 때
GitHub 호스티드 러너(`ubuntu-latest`, `windows-2025`)는 인터넷에서 실행되므로 사내 서버(211.231.121.41:9100)에
연결하지 못할 수 있습니다 (방화벽). 배포 단계에서 연결 시간 초과가 나면 **사내망 Windows PC를 self-hosted runner로** 씁니다.

1. 저장소 → **Settings → Actions → Runners → New self-hosted runner** → Windows 선택, 안내된 명령을 사내 PC에서 실행
   (서비스로 설치 권장: `config.cmd` 단계에서 "run as service" 선택).
2. 그 PC에 PowerShell 7(`pwsh`)과 빌드 도구(Go 또는 .NET 빌드 환경)를 설치합니다.
3. 배포 job만 사내 러너에서 돌립니다. 빌드는 클라우드에서 하고 결과물(artifact)을 넘겨받는 구성이 간단합니다.
   ```yaml
     publish:
       needs: build
       runs-on: [self-hosted, windows]
       if: github.ref == 'refs/heads/main'
       steps:
         - uses: actions/checkout@v4          # tools/Publish-FileServer.ps1
         - uses: actions/download-artifact@v4
           with: { name: ics-scm-windows, path: dist }
         - shell: pwsh
           env: { FS_SERVER: "${{ secrets.FS_SERVER }}", FS_TOKEN: "${{ secrets.FS_TOKEN }}" }
           run: ./tools/Publish-FileServer.ps1 -Program ics-scm -Name "ICS-SCM" -File dist/ics-scm.exe -SkipIfUnconfigured
   ```
4. 사내 러너는 그 PC가 GitHub에 **나가는 연결(HTTPS 443)** 만 있으면 동작합니다. 외부에서 사내로 포트를 열 필요는 없습니다.
   러너는 해당 저장소에만 등록하고, **공개 저장소에는 쓰지 마세요** (외부 포크의 PR 코드가 사내 PC에서 실행될 수 있음).
