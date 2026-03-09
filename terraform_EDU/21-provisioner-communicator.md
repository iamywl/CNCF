# 21. Provisioner & Communicator 시스템 심화

## 목차
1. [개요](#1-개요)
2. [Provisioner 아키텍처](#2-provisioner-아키텍처)
3. [Provisioner 인터페이스 분석](#3-provisioner-인터페이스-분석)
4. [Factory 패턴](#4-factory-패턴)
5. [Communicator 아키텍처](#5-communicator-아키텍처)
6. [SSH Communicator 심화](#6-ssh-communicator-심화)
7. [WinRM Communicator](#7-winrm-communicator)
8. [Retry & Backoff 메커니즘](#8-retry--backoff-메커니즘)
9. [SCP 파일 전송 프로토콜](#9-scp-파일-전송-프로토콜)
10. [Bastion Host 연결](#10-bastion-host-연결)
11. [KeepAlive 메커니즘](#11-keepalive-메커니즘)
12. [왜(Why) 이렇게 설계했나](#12-왜why-이렇게-설계했나)
13. [PoC 매핑](#13-poc-매핑)

---

## 1. 개요

Terraform의 **Provisioner**는 리소스 생성 직후 원격 또는 로컬에서 초기화 스크립트를 실행하는 메커니즘이다. **Communicator**는 Provisioner가 원격 호스트와 통신하기 위한 SSH/WinRM 추상화 계층이다.

```
소스 경로:
├── internal/provisioners/       # Provisioner 인터페이스 정의
│   ├── provisioner.go           # Interface, Request/Response 타입
│   ├── factory.go               # Factory 패턴
│   └── doc.go                   # 패키지 문서
├── internal/communicator/       # Communicator 추상화
│   ├── communicator.go          # Communicator 인터페이스 + Retry
│   ├── ssh/                     # SSH 구현체
│   │   ├── communicator.go      # SSH Communicator (901줄)
│   │   ├── provisioner.go       # SSH 설정 파싱
│   │   ├── password.go          # 키보드-인터랙티브 인증
│   │   └── http_proxy.go        # HTTP 프록시 지원
│   ├── winrm/                   # WinRM 구현체
│   ├── remote/                  # 원격 명령 구조
│   └── shared/                  # SSH/WinRM 공통 스키마
```

### 핵심 설계 결정

| 결정 | 이유 |
|------|------|
| 인터페이스 기반 추상화 | SSH와 WinRM을 동일한 인터페이스로 통일 |
| Factory 패턴 | 플러그인 프로세스에서 lazy-initialization 지원 |
| 지수 백오프 재시도 | 네트워크 불안정 상황에서 연결 신뢰성 확보 |
| SCP 프로토콜 직접 구현 | SFTP 대비 호환성 극대화 (레거시 시스템 지원) |
| KeepAlive 고루틴 | 장시간 프로비저닝 중 SSH 연결 유지 |

---

## 2. Provisioner 아키텍처

### 전체 흐름

```
┌─────────────────────────────────────────────────────────────────┐
│                    Terraform Apply 엔진                         │
│                                                                 │
│  ┌─────────────┐    ┌──────────────┐    ┌────────────────────┐ │
│  │ Resource     │───>│ Provisioner  │───>│ Communicator       │ │
│  │ Node         │    │ Interface    │    │ (SSH / WinRM)      │ │
│  │             │    │              │    │                    │ │
│  │ NodeApply   │    │ GetSchema()  │    │ Connect()          │ │
│  │ Execute()   │    │ Validate()   │    │ Start(cmd)         │ │
│  │             │    │ Provision()  │    │ Upload(path, r)    │ │
│  │             │    │ Stop()       │    │ UploadDir(dst,src) │ │
│  │             │    │ Close()      │    │ Disconnect()       │ │
│  └─────────────┘    └──────────────┘    └────────────────────┘ │
└─────────────────────────────────────────────────────────────────┘
```

### Provisioner 생명주기

```
1. Factory 호출 → Interface 인스턴스 생성
2. GetSchema() → 설정 스키마 획득
3. ValidateProvisionerConfig() → 설정 검증
4. ProvisionResource() → 실제 프로비저닝 실행
   ├── Communicator.Connect() → 원격 연결
   ├── Communicator.Upload() → 스크립트 업로드
   ├── Communicator.Start() → 명령 실행
   └── UIOutput.Output() → 실시간 출력
5. Stop() → 인터럽트 (SIGINT 수신 시)
6. Close() → 플러그인 프로세스 종료
```

---

## 3. Provisioner 인터페이스 분석

### `internal/provisioners/provisioner.go`

```go
// Interface는 Provisioner 플러그인이 구현해야 하는 메서드 집합이다.
type Interface interface {
    GetSchema() GetSchemaResponse
    ValidateProvisionerConfig(ValidateProvisionerConfigRequest) ValidateProvisionerConfigResponse
    ProvisionResource(ProvisionResourceRequest) ProvisionResourceResponse
    Stop() error
    Close() error
}
```

### Request/Response 패턴

Terraform의 Provider 시스템과 동일한 패턴으로 설계되어 있다:

```go
type ProvisionResourceRequest struct {
    Config     cty.Value       // 완전한 Provisioner 설정값
    Connection cty.Value       // 연결 정보 (host, user, password 등)
    UIOutput   UIOutput        // 실시간 출력 인터페이스
}

type ProvisionResourceResponse struct {
    Diagnostics tfdiags.Diagnostics  // 경고/에러 메시지
}
```

**왜 `cty.Value`를 사용하는가?**

HCL 표현식 평가 결과와 직접 연동하기 위해서다. HCL 파서가 `.tf` 파일을 파싱하면 `cty.Value`로 변환되는데, 이를 별도의 중간 변환 없이 곧바로 Provisioner에 전달할 수 있다. 이는 타입 안전성을 유지하면서도 직렬화/역직렬화 오버헤드를 줄인다.

### UIOutput 인터페이스

```go
type UIOutput interface {
    Output(string)
}
```

이 인터페이스는 의도적으로 단순하다. Provisioner는 장시간 실행되는 스크립트의 stdout을 실시간으로 사용자에게 보여줘야 하므로, 단방향 출력 채널만 필요하다. 양방향 통신이 필요없는 이유는 Provisioner 실행 중에 사용자 입력을 받는 시나리오가 없기 때문이다.

---

## 4. Factory 패턴

### `internal/provisioners/factory.go`

```go
type Factory func() (Interface, error)

func FactoryFixed(p Interface) Factory {
    return func() (Interface, error) {
        return p, nil
    }
}
```

### 왜 Factory 패턴인가?

Provisioner는 별도의 플러그인 프로세스로 실행될 수 있다. Factory는 두 가지 시나리오를 지원한다:

| 시나리오 | Factory 동작 |
|---------|-------------|
| 내장 Provisioner (local-exec) | `FactoryFixed()` — 이미 생성된 인스턴스 반환 |
| 플러그인 Provisioner | 새 프로세스 시작, gRPC 연결 수립 후 인스턴스 반환 |

`FactoryFixed`의 문서에 명시된 주의사항:

> Unlike usual factories, the exact same instance is returned for each call
> to the factory and so this must be used in only specialized situations where
> the caller can take care to either not mutate the given provider at all
> or to mutate it in ways that will not cause unexpected behavior for others
> holding the same reference.

이는 **동시성 안전** 문제를 호출자에게 위임하는 설계다. 내장 Provisioner의 경우 Terraform Core가 동시 실행을 조율하므로 안전하다.

---

## 5. Communicator 아키텍처

### 인터페이스 정의

`internal/communicator/communicator.go`에서 정의:

```go
type Communicator interface {
    Connect(provisioners.UIOutput) error   // 연결 수립
    Disconnect() error                     // 연결 종료
    Timeout() time.Duration                // 설정된 타임아웃
    ScriptPath() string                    // 스크립트 경로
    Start(*remote.Cmd) error               // 원격 명령 실행
    Upload(string, io.Reader) error        // 단일 파일 업로드
    UploadScript(string, io.Reader) error  // 실행 가능 스크립트 업로드
    UploadDir(string, string) error        // 디렉토리 업로드
}
```

### 연결 타입 선택

```go
func New(v cty.Value) (Communicator, error) {
    v, err := shared.ConnectionBlockSupersetSchema.CoerceValue(v)
    // ...
    switch connType {
    case "ssh", "":  // 기본값은 SSH
        return ssh.New(v)
    case "winrm":
        return winrm.New(v)
    default:
        return nil, fmt.Errorf("connection type '%s' not supported", connType)
    }
}
```

**왜 SSH가 기본값인가?**

Linux/Unix 서버가 인프라의 대다수를 차지하며, SSH가 사실상 표준 원격 접속 프로토콜이기 때문이다. Windows 서버만 WinRM이 필요하며, 최근 Windows도 OpenSSH를 지원하므로 SSH 우선이 합리적이다.

---

## 6. SSH Communicator 심화

### 구조체 (`internal/communicator/ssh/communicator.go`)

```go
type Communicator struct {
    connInfo        *connectionInfo    // 연결 정보 (host, port, user 등)
    client          *ssh.Client        // Go SSH 클라이언트
    config          *sshConfig         // SSH 설정
    conn            net.Conn           // TCP 연결
    cancelKeepAlive context.CancelFunc // KeepAlive 고루틴 취소
    lock            sync.Mutex         // 동시 접근 보호
}
```

### 연결 수립 흐름

```
Connect() 호출
    │
    ├── 기존 연결 정리 (conn, client nil로)
    │
    ├── UIOutput으로 연결 정보 출력
    │   ├── Host, User, 인증 방법
    │   ├── Bastion Host 정보 (있는 경우)
    │   └── Proxy 정보 (있는 경우)
    │
    ├── c.config.connection()으로 TCP 연결
    │
    ├── ssh.NewClientConn()으로 SSH 핸드셰이크
    │   └── 실패 시 WARN 로그 (재시도 가능)
    │
    ├── ssh.NewClient()로 SSH 클라이언트 생성
    │
    ├── SSH Agent 포워딩 (설정된 경우)
    │   ├── ForwardToAgent()
    │   └── RequestAgentForwarding()
    │
    └── KeepAlive 고루틴 시작
```

### 명령 실행 (`Start`)

```go
func (c *Communicator) Start(cmd *remote.Cmd) error {
    cmd.Init()
    session, err := c.newSession()
    // ...
    session.Stdin = cmd.Stdin
    session.Stdout = cmd.Stdout
    session.Stderr = cmd.Stderr

    // PTY 요청 (Unix만, noPty 아닌 경우)
    if !c.config.noPty && c.connInfo.TargetPlatform != TargetPlatformWindows {
        termModes := ssh.TerminalModes{
            ssh.ECHO:          0,      // 에코 비활성
            ssh.TTY_OP_ISPEED: 14400,  // 입력 속도
            ssh.TTY_OP_OSPEED: 14400,  // 출력 속도
        }
        session.RequestPty("xterm", 80, 40, termModes)
    }

    session.Start(strings.TrimSpace(cmd.Command) + "\n")

    // 비동기로 종료 대기
    go func() {
        defer session.Close()
        err := session.Wait()
        exitStatus := 0
        if err != nil {
            if exitErr, ok := err.(*ssh.ExitError); ok {
                exitStatus = exitErr.ExitStatus()
            }
        }
        cmd.SetExitStatus(exitStatus, err)
    }()
    return nil
}
```

**왜 PTY를 요청하는가?**

많은 원격 명령이 TTY가 없으면 다르게 동작한다 (예: sudo가 password prompt를 건너뜀). PTY를 할당하면 원격 명령이 일반 터미널 세션과 동일하게 동작한다. Windows에서는 PTY가 불필요하므로 제외한다.

### 세션 자동 복구

```go
func (c *Communicator) newSession() (session *ssh.Session, err error) {
    if c.client == nil {
        err = errors.New("ssh client is not connected")
    } else {
        session, err = c.client.NewSession()
    }
    if err != nil {
        // 자동 재연결 시도
        if err := c.Connect(nil); err != nil {
            return nil, err
        }
        return c.client.NewSession()
    }
    return session, nil
}
```

이 패턴은 네트워크 연결이 예기치 않게 끊어진 경우 자동으로 재연결을 시도한다.

---

## 7. WinRM Communicator

WinRM은 Windows Remote Management 프로토콜로, Windows 서버에서 PowerShell 명령을 원격 실행한다.

### SSH vs WinRM 비교

```
┌──────────────────────┬──────────────────────────┐
│       SSH            │        WinRM             │
├──────────────────────┼──────────────────────────┤
│ Linux/Unix 기본      │ Windows 전용             │
│ 포트 22              │ 포트 5985 (HTTP) /       │
│                      │ 5986 (HTTPS)             │
│ 키 기반 인증 지원    │ 인증서 또는 Basic Auth   │
│ SCP로 파일 전송      │ WinRM 명령으로 전송      │
│ PTY 지원             │ PTY 불필요               │
│ Bastion 지원         │ Bastion 미지원           │
└──────────────────────┴──────────────────────────┘
```

---

## 8. Retry & Backoff 메커니즘

### `internal/communicator/communicator.go`의 Retry 함수

```go
var maxBackoffDelay = 20 * time.Second
var initialBackoffDelay = time.Second

func Retry(ctx context.Context, f func() error) error {
    var errVal atomic.Value
    doneCh := make(chan struct{})

    go func() {
        defer close(doneCh)
        delay := time.Duration(0)
        for {
            select {
            case <-ctx.Done():
                return
            case <-time.After(delay):
            }

            err := f()
            // nil 또는 Fatal이면 종료
            done := false
            switch e := err.(type) {
            case nil:
                done = true
            case Fatal:
                err = e.FatalError()
                done = true
            }
            errVal.Store(errWrap{err})
            if done { return }

            // 지수 백오프
            delay *= 2
            if delay == 0 { delay = initialBackoffDelay }
            if delay > maxBackoffDelay { delay = maxBackoffDelay }
        }
    }()

    select {
    case <-ctx.Done():
    case <-doneCh:
    }
    // ...
}
```

### 백오프 진행

```
시도 1: 즉시
시도 2: 1초 대기
시도 3: 2초 대기
시도 4: 4초 대기
시도 5: 8초 대기
시도 6: 16초 대기
시도 7+: 20초 대기 (상한)
```

### Fatal 에러 인터페이스

```go
type Fatal interface {
    FatalError() error
}
```

이 인터페이스는 "이 에러는 재시도해도 복구 불가능"을 표현한다. SSH 인증 실패 중 일부(예: 호스트 키 불일치)는 Fatal로 표시되어 즉시 중단된다. 반면 네트워크 타임아웃은 재시도 가능하므로 일반 error로 반환된다.

### 왜 고루틴에서 Retry를 실행하는가?

Context 취소를 즉시 감지하기 위해서다. 사용자가 `Ctrl+C`를 누르면 Context가 취소되고, `select`에서 `<-ctx.Done()`가 즉시 선택된다. Retry 루프가 메인 고루틴에 있으면 현재 시도 중인 `f()` 호출이 완료될 때까지 취소를 감지할 수 없다.

---

## 9. SCP 파일 전송 프로토콜

### 업로드 흐름

```
Local                              Remote
  │                                  │
  │  "C0644 <size> <filename>\n"    │
  │ ──────────────────────────────> │
  │                                  │
  │  <status byte: 0=OK>            │
  │ <────────────────────────────── │
  │                                  │
  │  <file data bytes>               │
  │ ──────────────────────────────> │
  │                                  │
  │  "\x00" (종료 마커)              │
  │ ──────────────────────────────> │
  │                                  │
  │  <status byte: 0=OK>            │
  │ <────────────────────────────── │
```

### 디렉토리 업로드

```go
func scpUploadDirProtocol(name string, w io.Writer, r *bufio.Reader, f func() error) error {
    fmt.Fprintln(w, "D0755 0", name)  // 디렉토리 시작
    err := checkSCPStatus(r)
    if err := f(); err != nil {       // 하위 파일/디렉토리 업로드
        return err
    }
    fmt.Fprintln(w, "E")              // 디렉토리 종료
    return nil
}
```

### 파일 크기 미리 결정

```go
func scpUploadFile(dst string, src io.Reader, w io.Writer, r *bufio.Reader, size int64) error {
    if size == 0 {
        // 크기를 모르면 임시 파일에 복사하여 크기를 먼저 확인
        tf, _ := ioutil.TempFile("", "terraform-upload")
        io.Copy(tf, src)
        tf.Sync()
        tf.Seek(0, 0)
        fi, _ := tf.Stat()
        src = tf
        size = fi.Size()
    }
    fmt.Fprintln(w, "C0644", size, dst)
    // ...
}
```

**왜 SCP인가?** SFTP가 더 현대적이지만, SCP는 거의 모든 SSH 서버에서 기본 지원된다. 레거시 시스템이나 최소 설치 환경에서도 동작하므로 호환성이 중요한 Terraform에 적합하다.

---

## 10. Bastion Host 연결

### Bastion (Jump Host) 아키텍처

```
┌──────┐     ┌──────────────┐     ┌───────────────┐
│Local │────>│ Bastion Host │────>│ Target Host   │
│ PC   │ SSH │ (공개 IP)     │ SSH │ (사설 네트워크)│
└──────┘     └──────────────┘     └───────────────┘
```

### BastionConnectFunc

```go
func BastionConnectFunc(
    bProto, bAddr string,
    bConf *ssh.ClientConfig,
    proto, addr string,
    p *proxyInfo,
) func() (net.Conn, error) {
    return func() (net.Conn, error) {
        // 1단계: Bastion에 SSH 연결
        bastion, err := ssh.Dial(bProto, bAddr, bConf)

        // 2단계: Bastion을 통해 타겟에 연결
        conn, err := bastion.Dial(proto, addr)

        // bastionConn으로 감싸서 양쪽 모두 정리
        return &bastionConn{
            Conn:    conn,
            Bastion: bastion,
        }, nil
    }
}
```

### bastionConn

```go
type bastionConn struct {
    net.Conn
    Bastion *ssh.Client
}

func (c *bastionConn) Close() error {
    c.Conn.Close()         // 타겟 연결 닫기
    return c.Bastion.Close()  // Bastion 연결도 닫기
}
```

이 래퍼는 `Close()` 호출 시 타겟 연결과 Bastion 연결을 모두 정리한다. Go의 인터페이스 임베딩(`net.Conn`)을 활용하여 `Read`, `Write` 등 다른 메서드는 내부 `Conn`에 그대로 위임한다.

---

## 11. KeepAlive 메커니즘

### 동작 원리

```go
// 2초마다 keepalive 전송
var keepAliveInterval = 2 * time.Second
// 120초 이내에 응답 없으면 연결 사망으로 판정
var maxKeepAliveDelay = 120 * time.Second

go func() {
    // keepalive 요청 전송 고루틴
    go func() {
        t := time.NewTicker(keepAliveInterval)
        for {
            select {
            case <-t.C:
                _, _, err := sshClient.SendRequest("keepalive@terraform.io", true, nil)
                respCh <- err
            case <-ctx.Done():
                return
            }
        }
    }()

    // 응답 모니터링
    after := time.NewTimer(maxKeepAliveDelay)
    for {
        select {
        case err := <-respCh:
            if err != nil {
                sshConn.Close()  // 에러 발생 시 연결 종료
                return
            }
        case <-after.C:
            sshConn.Close()  // 타임아웃 시 연결 종료
            return
        }
        after.Reset(maxKeepAliveDelay)  // 응답 받을 때마다 타이머 리셋
    }
}()
```

### 왜 KeepAlive가 필요한가?

장시간 실행되는 Provisioner 스크립트(예: 대규모 패키지 설치)는 수십 분이 걸릴 수 있다. 이 동안 실제 데이터 전송이 없으면 중간 NAT/방화벽이 유휴 TCP 연결을 끊을 수 있다. KeepAlive 패킷은 연결을 "활성"으로 유지하면서, 동시에 서버 응답 여부를 확인하여 좀비 연결을 감지한다.

`"keepalive@terraform.io"`라는 커스텀 요청 이름은 SSH 프로토콜의 "global request"를 활용한다. 서버가 이를 인식하지 못해도 응답(success 또는 failure)을 보내므로, 연결 생존 확인이 가능하다.

---

## 12. 왜(Why) 이렇게 설계했나

### Q1: Provisioner가 Deprecated되는 추세인데 왜 아직 존재하는가?

Provisioner는 "escape hatch"로 설계되었다. Infrastructure as Code의 이상적인 모델에서는 모든 설정이 선언적이어야 하지만, 현실에서는:
- 레거시 시스템의 초기 부트스트랩이 필요
- VM 이미지에 포함되지 않은 설정 적용
- 클라우드 provider의 user_data만으로 부족한 경우

그러나 Provisioner는 멱등성을 보장하지 않고, State에 결과를 저장할 수 없으며, 실패 시 리소스를 "tainted"로 남기므로 운영 복잡성이 증가한다. 따라서 cloud-init, Packer, Ansible 등의 대안이 권장된다.

### Q2: 왜 Communicator를 별도 패키지로 분리했는가?

```
internal/provisioners/  ← 인터페이스만 정의
internal/communicator/  ← 통신 구현
```

이는 **관심사 분리(Separation of Concerns)** 원칙이다:
- Provisioner는 "무엇을 실행할지"를 정의
- Communicator는 "어떻게 연결할지"를 정의

이 분리 덕분에 새로운 통신 프로토콜(예: mTLS 기반 gRPC)을 추가해도 Provisioner 인터페이스를 변경할 필요가 없다.

### Q3: 왜 atomic.Value로 에러를 저장하는가?

```go
var errVal atomic.Value
```

Retry 함수에서 에러를 고루틴 간에 공유할 때 `atomic.Value`를 사용하는 이유:
- 뮤텍스 대비 오버헤드가 적음
- 읽기/쓰기가 단일 연산으로 원자적
- 대기 채널과 함께 사용하여 경합 조건 방지

### Q4: 왜 ScriptPath에 `%RAND%` 패턴을 사용하는가?

```go
func (c *Communicator) ScriptPath() string {
    return strings.Replace(
        c.connInfo.ScriptPath, "%RAND%",
        strconv.FormatInt(int64(randShared.Int31()), 10), -1)
}
```

동일한 호스트에 여러 Terraform 인스턴스가 동시에 프로비저닝할 때 스크립트 파일 이름 충돌을 방지한다. PID를 시드에 곱하는 것도 같은 이유다:

```go
randShared = rand.New(rand.NewSource(
    time.Now().UnixNano() * int64(os.Getpid())))
```

---

## 13. PoC 매핑

| PoC | 시뮬레이션 대상 |
|-----|---------------|
| poc-19-provisioner | Provisioner Interface + Factory 패턴, 연결 생명주기 |
| poc-20-communicator | SSH Communicator의 Retry/Backoff, SCP 프로토콜, KeepAlive |

---

## 참조 소스 파일

| 파일 | 줄수 | 핵심 내용 |
|------|------|----------|
| `internal/provisioners/provisioner.go` | 86 | Interface, UIOutput, Request/Response |
| `internal/provisioners/factory.go` | 23 | Factory, FactoryFixed |
| `internal/communicator/communicator.go` | 174 | Communicator 인터페이스, Retry, Backoff |
| `internal/communicator/ssh/communicator.go` | 901 | SSH 연결, SCP, KeepAlive, Bastion |
