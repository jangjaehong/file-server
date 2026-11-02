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

## 4. 배포 (빌드 PC, PowerShell)

`tools/Publish-FileServer.ps1`은 기존 `Publish-Internal.ps1`(IIS 폴더 복사)을 대체합니다.
버전은 지정하지 않으면 exe 옆의 `version.txt`(ICS-SCM) 또는 exe의 파일 버전(One TaskBar)에서 읽습니다.

```powershell
$env:FS_TOKEN = 'fst_...'   # 관리자 페이지 → API 토큰

# One TaskBar: 설치 파일 배포
.\Publish-FileServer.ps1 -Server http://211.231.121.41:9100 -Program onetaskbar -Name "One TaskBar" `
    -File ..\OneTaskBarAtWindows\release\OneTaskBar-Setup.exe -Notes "알림 배너 개선"

# One TaskBar: 회사 기본 설정(defaults.json) 교체 — Publish-Defaults.ps1 대체
.\Publish-FileServer.ps1 -Server http://211.231.121.41:9100 -Program onetaskbar -FixedFile .\defaults.json

# ICS-SCM: build.bat 결과물 배포
.\Publish-FileServer.ps1 -Server http://211.231.121.41:9100 -Program ics-scm -Name "ICS-SCM" `
    -File ..\ics-scm\dist\ics-scm.exe -NotesFile .\notes.md
```

| 옵션 | 설명 |
|---|---|
| `-Draft` | 업로드만 하고, 관리자 화면에서 검토 후 [배포] |
| `-Mandatory` | 필수 업데이트로 표시 (`latest.json`의 `mandatory`, `/api/v1/update`의 `mandatory`) |
| `-Channel beta` | 베타 채널 (`?channel=beta` 주소를 쓰는 PC만 받음) |
| `-Version 1.0.0.60` | 버전을 직접 지정 |

같은 버전 번호는 다시 올릴 수 없습니다. 고쳐서 다시 내보내려면 빌드 번호를 올려서 빌드하세요
(CI 실행 번호가 끝자리에 붙으므로 보통 자동으로 올라갑니다).

Linux/macOS 빌드 머신이나 CI에서는 같은 일을 `fsctl publish`로 할 수 있습니다.
```bash
fsctl publish -server http://211.231.121.41:9100 -token $FS_TOKEN -program ics-scm \
  -version 1.0.0.58 -notes "…" -file windows-amd64=dist/ics-scm.exe
```
