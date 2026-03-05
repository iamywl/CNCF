// poc-11-cli: Jenkins CLI & Remoting 시스템 시뮬레이션
//
// Jenkins CLI는 원격에서 Jenkins 서버에 명령을 보내고 실행 결과를 받는 시스템이다.
// PlainCLIProtocol은 바이너리 프레임 기반 양방향 통신 프로토콜로, HTTP/WebSocket 위에서 동작한다.
//
// 핵심 구조:
//   1. PlainCLIProtocol — 프레임 기반 바이너리 프로토콜 (Op 코드 + 데이터)
//   2. CLICommand — ExtensionPoint 기반 명령 레지스트리 (getName()으로 디스패치)
//   3. CLIAction — HTTP/WebSocket 엔드포인트에서 ServerSideImpl로 명령 처리
//   4. CLI.java — 클라이언트 진입점 (ClientSideImpl로 프레임 송수신)
//
// 참조 소스 코드:
//   - jenkins/cli/src/main/java/hudson/cli/PlainCLIProtocol.java
//     : 프레임 구조 — [int length][byte Op][Data...]
//     : Op enum: ARG(0), LOCALE(1), ENCODING(2), START(3), EXIT(4), STDIN(5),
//       END_STDIN(6), STDOUT(7), STDERR(8)
//     : EitherSide.send() — synchronized로 프레임 직렬화 후 전송
//     : FramedReader.run() — readInt() → handle() 루프로 프레임 수신
//   - jenkins/core/src/main/java/hudson/cli/CLICommand.java
//     : ExtensionPoint 기반 추상 클래스
//     : getName() — 클래스명에서 "FooBarCommand" → "foo-bar" 변환
//     : main(List<String> args, Locale, InputStream, PrintStream, PrintStream) → int
//     : clone(String name) — 이름으로 명령 조회 후 createClone()
//     : run() — 서브클래스가 구현하는 실제 실행 로직
//   - jenkins/core/src/main/java/hudson/cli/CLIAction.java
//     : @Extension @Symbol("cli") — HTTP/WebSocket 엔드포인트
//     : ServerSideImpl — PlainCLIProtocol.ServerSide 구현
//     : onArg()/onLocale()/onEncoding()/onStart() 콜백으로 프레임 처리
//     : run() — args에서 commandName 추출 → CLICommand.clone() → command.main() → sendExit()
//   - jenkins/cli/src/main/java/hudson/cli/CLI.java
//     : main() 진입점 — -http, -webSocket, -ssh 모드 선택
//     : ClientSideImpl — PlainCLIProtocol.ClientSide 구현
//     : start() — sendArg() → sendEncoding() → sendLocale() → sendStart()
//     : onExit()/onStdout()/onStderr() 콜백
//   - jenkins/core/src/main/java/hudson/cli/BuildCommand.java
//     : @Extension, CLICommand 상속, @Argument/@Option으로 인자 정의
//   - jenkins/core/src/main/java/hudson/cli/ListJobsCommand.java
//     : 동일 패턴, Jenkins.get().getItems()로 작업 목록 반환
//   - jenkins/core/src/main/java/hudson/cli/HelpCommand.java
//     : CLICommand.all()로 전체 명령 순회, getName()/getShortDescription() 출력
//   - jenkins/core/src/main/java/hudson/cli/WhoAmICommand.java
//     : Jenkins.getAuthentication2()로 현재 인증 정보 출력
//
// 실행: go run main.go

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 1. Op — PlainCLIProtocol의 오퍼레이션 코드
// ============================================================================
// jenkins/cli/src/main/java/hudson/cli/PlainCLIProtocol.java 55~80행:
//
//   private enum Op {
//       ARG(true),       // 0 — 클라이언트→서버: UTF-8 명령명 또는 인자
//       LOCALE(true),    // 1 — 클라이언트→서버: 로케일 식별자
//       ENCODING(true),  // 2 — 클라이언트→서버: 클라이언트 인코딩
//       START(true),     // 3 — 클라이언트→서버: 명령 실행 시작
//       EXIT(false),     // 4 — 서버→클라이언트: 종료 코드 (int)
//       STDIN(true),     // 5 — 클라이언트→서버: stdin 데이터 청크
//       END_STDIN(true), // 6 — 클라이언트→서버: stdin EOF
//       STDOUT(false),   // 7 — 서버→클라이언트: stdout 데이터 청크
//       STDERR(false);   // 8 — 서버→클라이언트: stderr 데이터 청크
//       final boolean clientSide;
//   }
//
// clientSide=true인 Op은 클라이언트가 서버로 보내고,
// clientSide=false인 Op은 서버가 클라이언트로 보낸다.

type Op byte

const (
	OpARG      Op = 0 // 클라이언트→서버: 명령 인자
	OpLOCALE   Op = 1 // 클라이언트→서버: 로케일
	OpENCODING Op = 2 // 클라이언트→서버: 인코딩
	OpSTART    Op = 3 // 클라이언트→서버: 명령 실행 시작
	OpEXIT     Op = 4 // 서버→클라이언트: 종료 코드
	OpSTDIN    Op = 5 // 클라이언트→서버: stdin 데이터
	OpENDSTDIN Op = 6 // 클라이언트→서버: stdin EOF
	OpSTDOUT   Op = 7 // 서버→클라이언트: stdout 데이터
	OpSTDERR   Op = 8 // 서버→클라이언트: stderr 데이터
)

// opNames는 Op 코드의 사람이 읽을 수 있는 이름 매핑
var opNames = map[Op]string{
	OpARG: "ARG", OpLOCALE: "LOCALE", OpENCODING: "ENCODING",
	OpSTART: "START", OpEXIT: "EXIT", OpSTDIN: "STDIN",
	OpENDSTDIN: "END_STDIN", OpSTDOUT: "STDOUT", OpSTDERR: "STDERR",
}

// isClientSide는 이 Op이 클라이언트에서 서버로 보내는 것인지 판별한다.
// PlainCLIProtocol.java의 Op(boolean clientSide) 생성자에 대응한다.
func (o Op) isClientSide() bool {
	switch o {
	case OpARG, OpLOCALE, OpENCODING, OpSTART, OpSTDIN, OpENDSTDIN:
		return true
	default:
		return false
	}
}

func (o Op) String() string {
	if name, ok := opNames[o]; ok {
		return name
	}
	return fmt.Sprintf("UNKNOWN(%d)", byte(o))
}

// ============================================================================
// 2. 프레임 인코딩/디코딩 — PlainCLIProtocol.FramedOutput / FramedReader
// ============================================================================
// PlainCLIProtocol.java 86~167행:
//
// 프레임 구조:
//   [4 bytes: int length] [1 byte: Op] [length bytes: data]
//   length는 Op 바이트를 제외한 데이터 길이 (즉, 실제 전송은 length+1 바이트)
//
// FramedOutput.send():
//   dos.writeInt(data.length - 1);  // Op 바이트를 빼고 길이 기록
//   dos.write(data);                // data[0]=Op, data[1:]=실제 데이터
//   dos.flush();
//
// FramedReader.run():
//   while (true) {
//       framelen = dis.readInt();       // 길이 읽기
//       side.handle(new DataInputStream(  // Op+데이터를 BoundedInputStream으로 감싸서 처리
//           new BoundedInputStream(dis, framelen + 1)));
//   }

// Frame은 PlainCLIProtocol의 단일 프레임을 나타낸다
type Frame struct {
	Op   Op     // 오퍼레이션 코드
	Data []byte // Op별 데이터 (Op 바이트 미포함)
}

// encodeFrame은 Frame을 바이너리로 직렬화한다.
// PlainCLIProtocol.FramedOutput.send()에 대응:
//   data[0] = (byte) op.ordinal();  // EitherSide.send()에서 Op을 data[0]에 넣음
//   dos.writeInt(data.length - 1);  // Op 빼고 길이
//   dos.write(data);                // Op + 실제 데이터
func encodeFrame(f Frame) []byte {
	// EitherSide.send(Op op, byte[] chunk, int off, int len):
	//   byte[] data = new byte[len + 1];
	//   data[0] = (byte) op.ordinal();
	//   System.arraycopy(chunk, off, data, 1, len);
	//   out.send(data);
	//
	// FramedOutput.send(byte[] data):
	//   dos.writeInt(data.length - 1);  // length 필드 = 데이터 크기 (Op 미포함)
	//   dos.write(data);                // Op + data

	dataLen := len(f.Data) // Op 바이트를 뺀 순수 데이터 길이
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, int32(dataLen)) // length 필드 (Big-endian int)
	buf.WriteByte(byte(f.Op))                           // Op 바이트
	buf.Write(f.Data)                                   // 실제 데이터
	return buf.Bytes()
}

// decodeFrame은 바이너리 스트림에서 하나의 Frame을 읽는다.
// PlainCLIProtocol.FramedReader.run()에 대응:
//   framelen = dis.readInt();
//   side.handle(new DataInputStream(new BoundedInputStream(dis, framelen + 1)));
func decodeFrame(r io.Reader) (Frame, error) {
	var length int32
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		return Frame{}, err
	}
	if length < 0 {
		return Frame{}, fmt.Errorf("corrupt stream: negative frame length %d", length)
	}

	// framelen + 1 바이트 읽기 (Op 1바이트 + 데이터 length 바이트)
	payload := make([]byte, length+1)
	if _, err := io.ReadFull(r, payload); err != nil {
		return Frame{}, err
	}

	op := Op(payload[0])
	data := payload[1:]
	return Frame{Op: op, Data: data}, nil
}

// encodeStringData는 문자열을 Java의 DataOutputStream.writeUTF() 호환 형식으로 인코딩한다.
// PlainCLIProtocol.EitherSide.send(Op, String):
//   ByteArrayOutputStream buf = new ByteArrayOutputStream();
//   new DataOutputStream(buf).writeUTF(text);
//   send(op, buf.toByteArray());
// Java writeUTF: [2 bytes: UTF-8 length (big-endian unsigned short)] [UTF-8 bytes]
func encodeStringData(s string) []byte {
	utf8Bytes := []byte(s)
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, uint16(len(utf8Bytes)))
	buf.Write(utf8Bytes)
	return buf.Bytes()
}

// decodeStringData는 Java writeUTF 형식의 문자열을 디코딩한다.
func decodeStringData(data []byte) string {
	if len(data) < 2 {
		return ""
	}
	strLen := binary.BigEndian.Uint16(data[:2])
	if int(strLen)+2 > len(data) {
		return string(data[2:])
	}
	return string(data[2 : 2+strLen])
}

// encodeIntData는 int32를 Big-endian 4바이트로 인코딩한다.
// PlainCLIProtocol.EitherSide.send(Op, int):
//   ByteArrayOutputStream baos = new ByteArrayOutputStream(4);
//   new DataOutputStream(baos).writeInt(v);
//   send(op, baos.toByteArray());
func encodeIntData(v int32) []byte {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, v)
	return buf.Bytes()
}

// decodeIntData는 Big-endian 4바이트를 int32로 디코딩한다.
func decodeIntData(data []byte) int32 {
	if len(data) < 4 {
		return -1
	}
	return int32(binary.BigEndian.Uint32(data[:4]))
}

// ============================================================================
// 3. CLICommand — 명령 인터페이스 및 레지스트리
// ============================================================================
// jenkins/core/src/main/java/hudson/cli/CLICommand.java:
//
// public abstract class CLICommand implements ExtensionPoint, Cloneable {
//     public String getName() { ... }          // 클래스명 → kebab-case 변환
//     public abstract String getShortDescription();
//     public int main(List<String> args, ...) { ... }  // → run() 호출
//     protected abstract int run() throws Exception;
//     public static CLICommand clone(String name) { ... }  // 레지스트리 조회
// }
//
// getName() 변환 규칙 (178~188행):
//   - 클래스명에서 패키지 제거, "Command" 접미사 제거
//   - "FooBarZot" → "foo-bar-zot" (CamelCase → kebab-case)
//   - replaceAll("([a-z0-9])([A-Z])", "$1-$2").toLowerCase(Locale.ENGLISH)

// CLICommand는 Jenkins CLI 명령의 인터페이스이다.
// 실제 Jenkins에서는 ExtensionPoint를 구현하여 플러그인이 새 명령을 추가할 수 있다.
type CLICommand interface {
	// GetName은 명령의 이름을 반환한다.
	// CLICommand.getName() — 클래스명에서 자동 유도되거나 직접 지정
	GetName() string

	// GetShortDescription은 명령의 간단한 설명을 반환한다.
	// 실제: HelpCommand에서 전체 명령 목록 출력 시 사용
	GetShortDescription() string

	// Run은 명령을 실행하고 종료 코드를 반환한다.
	// CLICommand.main() → run() 호출 체인에 대응
	// args: 명령 인자, auth: 인증 정보
	// stdout, stderr: 출력 스트림 (바이트 슬라이스로 반환)
	Run(args []string, auth *Authentication) (exitCode int, stdout string, stderr string)
}

// Authentication은 CLI 연결의 인증 정보를 나타낸다.
// 실제: org.springframework.security.core.Authentication
// CLICommand.getTransportAuthentication2()에서 반환
type Authentication struct {
	Name        string   // 사용자명
	Authorities []string // 부여된 권한 목록
}

// ============================================================================
// 4. CommandRegistry — ExtensionPoint 기반 명령 레지스트리
// ============================================================================
// CLICommand.java 539~551행:
//
//   public static ExtensionList<CLICommand> all() {
//       return ExtensionList.lookup(CLICommand.class);
//   }
//
//   public static CLICommand clone(String name) {
//       for (CLICommand cmd : all())
//           if (name.equals(cmd.getName()))
//               return cmd.createClone();
//       return null;
//   }
//
// Jenkins의 @Extension 어노테이션으로 등록된 모든 CLICommand 구현체를
// ExtensionList에서 조회한다. 이름이 일치하는 명령의 클론을 생성하여 반환.

// CommandRegistry는 CLICommand를 이름으로 관리하는 레지스트리이다.
type CommandRegistry struct {
	mu       sync.RWMutex
	commands map[string]CLICommand
}

// NewCommandRegistry는 새 CommandRegistry를 생성한다.
func NewCommandRegistry() *CommandRegistry {
	return &CommandRegistry{
		commands: make(map[string]CLICommand),
	}
}

// Register는 명령을 레지스트리에 등록한다.
// Jenkins의 @Extension 어노테이션 + ExtensionList.lookup()에 대응
func (r *CommandRegistry) Register(cmd CLICommand) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands[cmd.GetName()] = cmd
}

// Clone은 이름으로 명령을 조회한다.
// CLICommand.clone(String name)에 대응
func (r *CommandRegistry) Clone(name string) CLICommand {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.commands[name]
}

// All은 등록된 모든 명령을 이름순으로 반환한다.
// CLICommand.all()에 대응
func (r *CommandRegistry) All() []CLICommand {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cmds := make([]CLICommand, 0, len(r.commands))
	for _, cmd := range r.commands {
		cmds = append(cmds, cmd)
	}
	sort.Slice(cmds, func(i, j int) bool {
		return cmds[i].GetName() < cmds[j].GetName()
	})
	return cmds
}

// ============================================================================
// 5. 내장 명령 구현 — help, version, who-am-i, build, list-jobs, safe-restart
// ============================================================================

// --- HelpCommand ---
// jenkins/core/src/main/java/hudson/cli/HelpCommand.java:
// @Extension
// public class HelpCommand extends CLICommand {
//     protected int run() {
//         for (CLICommand c : commands.values()) {
//             stderr.println("  " + c.getName());
//             stderr.println("    " + c.getShortDescription());
//         }
//         return 0;
//     }
// }

type HelpCommand struct {
	registry *CommandRegistry
}

func (c *HelpCommand) GetName() string             { return "help" }
func (c *HelpCommand) GetShortDescription() string { return "모든 사용 가능한 명령 목록을 출력합니다" }
func (c *HelpCommand) Run(args []string, auth *Authentication) (int, string, string) {
	var out strings.Builder

	// 특정 명령의 도움말 요청
	if len(args) > 0 {
		cmd := c.registry.Clone(args[0])
		if cmd == nil {
			// HelpCommand.java 79~84행: showAllCommands() 후 AbortException
			return 5, "", fmt.Sprintf("ERROR: No such command %s", args[0])
		}
		out.WriteString(fmt.Sprintf("  %s\n    %s\n", cmd.GetName(), cmd.GetShortDescription()))
		return 0, out.String(), ""
	}

	// 전체 명령 목록 — HelpCommand.showAllCommands()
	for _, cmd := range c.registry.All() {
		out.WriteString(fmt.Sprintf("  %-20s %s\n", cmd.GetName(), cmd.GetShortDescription()))
	}
	return 0, out.String(), ""
}

// --- VersionCommand ---
// jenkins-cli.jar -version → CLI.computeVersion()
// 서버 측: VersionCommand는 Jenkins.getVersion() 반환

type VersionCommand struct{}

func (c *VersionCommand) GetName() string             { return "version" }
func (c *VersionCommand) GetShortDescription() string { return "Jenkins 버전을 출력합니다" }
func (c *VersionCommand) Run(args []string, auth *Authentication) (int, string, string) {
	return 0, "Jenkins ver. 2.462.1-SIMULATION\n", ""
}

// --- WhoAmICommand ---
// jenkins/core/src/main/java/hudson/cli/WhoAmICommand.java:
// @Extension
// public class WhoAmICommand extends CLICommand {
//     protected int run() {
//         Authentication a = Jenkins.getAuthentication2();
//         stdout.println("Authenticated as: " + a.getName());
//         stdout.println("Authorities:");
//         for (GrantedAuthority ga : a.getAuthorities())
//             stdout.println("  " + ga.getAuthority());
//         return 0;
//     }
// }

type WhoAmICommand struct{}

func (c *WhoAmICommand) GetName() string             { return "who-am-i" }
func (c *WhoAmICommand) GetShortDescription() string { return "현재 인증 정보와 권한을 출력합니다" }
func (c *WhoAmICommand) Run(args []string, auth *Authentication) (int, string, string) {
	if auth == nil {
		return 0, "Authenticated as: anonymous\nAuthorities:\n  anonymous\n", ""
	}
	var out strings.Builder
	out.WriteString(fmt.Sprintf("Authenticated as: %s\n", auth.Name))
	out.WriteString("Authorities:\n")
	for _, a := range auth.Authorities {
		out.WriteString(fmt.Sprintf("  %s\n", a))
	}
	return 0, out.String(), ""
}

// --- BuildCommand ---
// jenkins/core/src/main/java/hudson/cli/BuildCommand.java:
// @Extension
// public class BuildCommand extends CLICommand {
//     @Argument(metaVar = "JOB", usage = "Name of the job to build", required = true)
//     public Job<?, ?> job;
//     protected int run() {
//         job.checkPermission(Item.BUILD);
//         // ... 파라미터 처리, 큐에 추가, 대기 ...
//         return 0;
//     }
// }

type BuildCommand struct {
	jobs map[string]*JobInfo // 시뮬레이션용 작업 저장소
}

type JobInfo struct {
	Name       string
	Buildable  bool
	BuildCount int
}

func (c *BuildCommand) GetName() string             { return "build" }
func (c *BuildCommand) GetShortDescription() string { return "작업을 빌드합니다 (선택적으로 완료까지 대기)" }
func (c *BuildCommand) Run(args []string, auth *Authentication) (int, string, string) {
	if len(args) == 0 {
		return 2, "", "ERROR: Argument \"JOB\" is required\n"
	}

	jobName := args[0]
	job, exists := c.jobs[jobName]
	if !exists {
		return 3, "", fmt.Sprintf("ERROR: No such job '%s'\n", jobName)
	}

	if !job.Buildable {
		// BuildCommand.java 159행: isDisabled() 체크
		return 4, "", fmt.Sprintf("ERROR: %s is disabled\n", jobName)
	}

	// 권한 체크 시뮬레이션 — CLICommand.main()에서 Jenkins.READ 권한 확인 후
	// BuildCommand.run()에서 Item.BUILD 권한 확인
	if auth == nil || auth.Name == "anonymous" {
		return 6, "", "ERROR: anonymous is missing the Job/Build permission\n"
	}

	job.BuildCount++
	return 0, fmt.Sprintf("Started %s #%d\nCompleted %s #%d : SUCCESS\n",
		jobName, job.BuildCount, jobName, job.BuildCount), ""
}

// --- ListJobsCommand ---
// jenkins/core/src/main/java/hudson/cli/ListJobsCommand.java:
// @Extension
// public class ListJobsCommand extends CLICommand {
//     protected int run() {
//         for (TopLevelItem item : jobs)
//             stdout.println(item.getName());
//         return 0;
//     }
// }

type ListJobsCommand struct {
	jobs map[string]*JobInfo
}

func (c *ListJobsCommand) GetName() string             { return "list-jobs" }
func (c *ListJobsCommand) GetShortDescription() string { return "모든 작업을 해당 뷰/항목 그룹에서 나열합니다" }
func (c *ListJobsCommand) Run(args []string, auth *Authentication) (int, string, string) {
	var out strings.Builder
	names := make([]string, 0, len(c.jobs))
	for name := range c.jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out.WriteString(name + "\n")
	}
	return 0, out.String(), ""
}

// --- SafeRestartCommand ---
// Jenkins safe-restart: 대기 중인 빌드 완료 후 재시작

type SafeRestartCommand struct{}

func (c *SafeRestartCommand) GetName() string             { return "safe-restart" }
func (c *SafeRestartCommand) GetShortDescription() string { return "빌드 완료 후 Jenkins를 안전하게 재시작합니다" }
func (c *SafeRestartCommand) Run(args []string, auth *Authentication) (int, string, string) {
	if auth == nil || auth.Name == "anonymous" {
		return 6, "", "ERROR: anonymous is missing the Overall/Administer permission\n"
	}
	return 0, "Jenkins가 안전하게 재시작됩니다...\n", ""
}

// ============================================================================
// 6. CLIServer — PlainCLIProtocol 서버 측 구현
// ============================================================================
// jenkins/core/src/main/java/hudson/cli/CLIAction.java 247~358행:
//
// static class ServerSideImpl extends PlainCLIProtocol.ServerSide {
//     private final List<String> args = new ArrayList<>();
//     private Locale locale; private Charset encoding;
//     private final Authentication authentication;
//
//     void onArg(String text)     { args.add(text); }
//     void onLocale(String text)  { locale = ...; }
//     void onEncoding(String text){ encoding = Charset.forName(text); }
//     void onStart()              { ready(); }  // 모든 인자 수신 완료
//
//     void run() {
//         String commandName = args.getFirst();
//         CLICommand command = CLICommand.clone(commandName);
//         command.setTransportAuth2(authentication);
//         int exit = command.main(args.subList(1, args.size()), locale, stdin, stdout, stderr);
//         stdout.flush();
//         sendExit(exit);
//     }
// }

// CLIServer는 TCP 소켓 위에서 PlainCLIProtocol을 처리하는 서버이다.
type CLIServer struct {
	registry *CommandRegistry
	listener net.Listener
	wg       sync.WaitGroup
}

// NewCLIServer는 지정된 주소에서 리스닝하는 CLI 서버를 생성한다.
func NewCLIServer(addr string, registry *CommandRegistry) (*CLIServer, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &CLIServer{
		registry: registry,
		listener: ln,
	}, nil
}

// Addr는 서버의 리스닝 주소를 반환한다.
func (s *CLIServer) Addr() string {
	return s.listener.Addr().String()
}

// Serve는 클라이언트 연결을 수락하고 처리한다.
func (s *CLIServer) Serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return // 서버 종료
		}
		s.wg.Add(1)
		go s.handleConnection(conn)
	}
}

// Close는 서버를 종료한다.
func (s *CLIServer) Close() {
	s.listener.Close()
	s.wg.Wait()
}

// handleConnection은 단일 CLI 연결을 처리한다.
// CLIAction.ServerSideImpl.run()에 대응
func (s *CLIServer) handleConnection(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	// --- 프레임 수신 단계: 클라이언트 Op 처리 ---
	// ServerSideImpl의 콜백에 대응:
	//   onArg() → args에 추가
	//   onLocale() → locale 설정
	//   onEncoding() → encoding 설정
	//   onStart() → ready() 호출하여 명령 실행 시작

	var args []string
	var locale, encoding string
	auth := &Authentication{Name: "anonymous", Authorities: []string{"anonymous"}}

	// 간단한 인증 헤더 처리 시뮬레이션
	// 실제 Jenkins에서는 CLIAction.doWs()에서 Jenkins.getAuthentication2()로 HTTP 인증 추출
	// 여기서는 첫 번째 프레임 전에 인증 프레임을 추가로 처리 (시뮬레이션용 확장)

	started := false
	for !started {
		frame, err := decodeFrame(conn)
		if err != nil {
			return
		}

		// PlainCLIProtocol.ServerSide.handle()에 대응
		switch frame.Op {
		case OpARG:
			// onArg(dis.readUTF()) — Java writeUTF 형식 디코딩
			args = append(args, decodeStringData(frame.Data))
		case OpLOCALE:
			locale = decodeStringData(frame.Data)
			_ = locale // 시뮬레이션에서는 사용하지 않음
		case OpENCODING:
			encoding = decodeStringData(frame.Data)
			_ = encoding
		case OpSTART:
			// onStart() → ready() — 모든 인자 수신 완료, 명령 실행 시작
			started = true
		default:
			// 알 수 없는 Op은 무시 (ProtocolException 대신 경고)
		}
	}

	// --- 명령 디스패치 ---
	// CLIAction.ServerSideImpl.run() 329~348행:
	//   String commandName = args.getFirst();
	//   CLICommand command = CLICommand.clone(commandName);
	//   if (command == null) { stderr.println("No such command"); sendExit(2); return; }
	//   command.setTransportAuth2(authentication);
	//   int exit = command.main(args.subList(1, args.size()), locale, stdin, stdout, stderr);
	//   stdout.flush();
	//   sendExit(exit);

	if len(args) == 0 {
		sendFrame(conn, Frame{Op: OpSTDERR, Data: []byte("ERROR: Connection closed before arguments received\n")})
		sendFrame(conn, Frame{Op: OpEXIT, Data: encodeIntData(2)})
		return
	}

	commandName := args[0]
	cmdArgs := args[1:]

	// 인증 시뮬레이션: args에서 -auth 옵션 파싱
	// 실제: CLI.java에서 -auth user:token으로 Authorization 헤더 설정
	// CLIAction에서 Jenkins.getAuthentication2()로 추출
	for i := 0; i < len(cmdArgs)-1; i++ {
		if cmdArgs[i] == "-auth" {
			parts := strings.SplitN(cmdArgs[i+1], ":", 2)
			if len(parts) == 2 {
				auth = &Authentication{
					Name:        parts[0],
					Authorities: []string{"authenticated", "admin"},
				}
			}
			cmdArgs = append(cmdArgs[:i], cmdArgs[i+2:]...)
			break
		}
	}

	cmd := s.registry.Clone(commandName)
	if cmd == nil {
		msg := fmt.Sprintf("ERROR: No such command %s\n", commandName)
		sendFrame(conn, Frame{Op: OpSTDERR, Data: []byte(msg)})
		sendFrame(conn, Frame{Op: OpEXIT, Data: encodeIntData(2)})
		return
	}

	// 명령 실행 — CLICommand.main() → run()
	exitCode, stdout, stderr := cmd.Run(cmdArgs, auth)

	// 결과 전송 — ServerSide.streamStdout()/streamStderr()/sendExit()
	if stdout != "" {
		sendFrame(conn, Frame{Op: OpSTDOUT, Data: []byte(stdout)})
	}
	if stderr != "" {
		sendFrame(conn, Frame{Op: OpSTDERR, Data: []byte(stderr)})
	}
	sendFrame(conn, Frame{Op: OpEXIT, Data: encodeIntData(int32(exitCode))})
}

// sendFrame은 프레임을 연결에 쓴다.
func sendFrame(w io.Writer, f Frame) error {
	_, err := w.Write(encodeFrame(f))
	return err
}

// ============================================================================
// 7. CLIClient — PlainCLIProtocol 클라이언트 측 구현
// ============================================================================
// jenkins/cli/src/main/java/hudson/cli/CLI.java 452~523행:
//
// private static final class ClientSideImpl extends PlainCLIProtocol.ClientSide {
//     void start(List<String> args) {
//         for (String arg : args) sendArg(arg);
//         sendEncoding(Charset.defaultCharset().name());
//         sendLocale(Locale.getDefault().toString());
//         sendStart();
//     }
//
//     void onExit(int code)           { this.exit = code; finished(); }
//     void onStdout(byte[] chunk)     { System.out.write(chunk); }
//     void onStderr(byte[] chunk)     { System.err.write(chunk); }
// }

// CLIClient는 CLI 서버에 연결하여 명령을 실행하는 클라이언트이다.
type CLIClient struct {
	conn net.Conn
}

// NewCLIClient는 서버에 연결하는 CLI 클라이언트를 생성한다.
func NewCLIClient(addr string) (*CLIClient, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	return &CLIClient{conn: conn}, nil
}

// Execute는 명령을 서버에 전송하고 결과를 수신한다.
// CLI.ClientSideImpl.start() + exit()에 대응
func (c *CLIClient) Execute(args []string) (exitCode int, stdout, stderr string) {
	defer c.conn.Close()

	// --- 프레임 전송 단계 ---
	// ClientSideImpl.start():
	//   for (String arg : args) sendArg(arg);
	//   sendEncoding(Charset.defaultCharset().name());
	//   sendLocale(Locale.getDefault().toString());
	//   sendStart();

	for _, arg := range args {
		sendFrame(c.conn, Frame{Op: OpARG, Data: encodeStringData(arg)})
	}
	sendFrame(c.conn, Frame{Op: OpENCODING, Data: encodeStringData("UTF-8")})
	sendFrame(c.conn, Frame{Op: OpLOCALE, Data: encodeStringData("ko_KR")})
	sendFrame(c.conn, Frame{Op: OpSTART, Data: nil})

	// --- 프레임 수신 단계 ---
	// ClientSideImpl의 콜백:
	//   onExit(code) → exit 설정, finished()
	//   onStdout(chunk) → System.out.write(chunk)
	//   onStderr(chunk) → System.err.write(chunk)

	var stdoutBuf, stderrBuf strings.Builder
	exitCode = -1

	for {
		frame, err := decodeFrame(c.conn)
		if err != nil {
			break
		}

		switch frame.Op {
		case OpSTDOUT:
			stdoutBuf.Write(frame.Data)
		case OpSTDERR:
			stderrBuf.Write(frame.Data)
		case OpEXIT:
			exitCode = int(decodeIntData(frame.Data))
			return exitCode, stdoutBuf.String(), stderrBuf.String()
		}
	}

	return exitCode, stdoutBuf.String(), stderrBuf.String()
}

// ============================================================================
// 8. Remoting Channel 시뮬레이션 (간략)
// ============================================================================
// Jenkins Remoting (hudson.remoting.Channel)은 양방향 직렬화 기반 RPC 채널이다.
// 현재 CLI에서는 더 이상 사용하지 않지만(-remoting 모드 제거됨),
// Jenkins 에이전트 통신에서는 여전히 핵심이다.
//
// 핵심 구조:
//   - Request<RSP,EXC> extends Command: 직렬화된 요청
//   - Channel: 양방향 스트림 위에서 Command 송수신
//   - 요청 ID로 응답 매칭 (pendingCalls 맵)
//
// 여기서는 간단한 RPC 패턴만 시뮬레이션한다.

// RemotingRequest는 Remoting 채널의 요청을 나타낸다.
type RemotingRequest struct {
	ID     int
	Method string
	Args   []string
}

// RemotingResponse는 Remoting 채널의 응답을 나타낸다.
type RemotingResponse struct {
	ID     int
	Result string
	Error  string
}

// RemotingChannel은 간단한 양방향 RPC 채널을 시뮬레이션한다.
type RemotingChannel struct {
	mu           sync.Mutex
	nextID       int
	pendingCalls map[int]chan RemotingResponse
	handlers     map[string]func([]string) (string, error)
}

// NewRemotingChannel은 새 Remoting 채널을 생성한다.
func NewRemotingChannel() *RemotingChannel {
	return &RemotingChannel{
		pendingCalls: make(map[int]chan RemotingResponse),
		handlers:     make(map[string]func([]string) (string, error)),
	}
}

// RegisterHandler는 RPC 메서드 핸들러를 등록한다.
func (ch *RemotingChannel) RegisterHandler(method string, handler func([]string) (string, error)) {
	ch.handlers[method] = handler
}

// Call은 동기식 RPC 호출을 수행한다.
func (ch *RemotingChannel) Call(method string, args []string) RemotingResponse {
	ch.mu.Lock()
	id := ch.nextID
	ch.nextID++
	respCh := make(chan RemotingResponse, 1)
	ch.pendingCalls[id] = respCh
	ch.mu.Unlock()

	// 요청 처리 (로컬 시뮬레이션)
	go func() {
		handler, exists := ch.handlers[method]
		var resp RemotingResponse
		resp.ID = id
		if !exists {
			resp.Error = fmt.Sprintf("no handler for method: %s", method)
		} else {
			result, err := handler(args)
			if err != nil {
				resp.Error = err.Error()
			} else {
				resp.Result = result
			}
		}

		ch.mu.Lock()
		if ch, ok := ch.pendingCalls[id]; ok {
			ch <- resp
		}
		ch.mu.Unlock()
	}()

	return <-respCh
}

// ============================================================================
// 9. 데모 실행
// ============================================================================

func main() {
	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Println("  Jenkins CLI & Remoting 시뮬레이션")
	fmt.Println("  PlainCLIProtocol 프레임 기반 통신 + CLICommand 레지스트리")
	fmt.Println("=" + strings.Repeat("=", 79))

	// -----------------------------------------------------------------------
	// 데모 1: 프레임 인코딩/디코딩 시각화
	// -----------------------------------------------------------------------
	fmt.Println("\n[데모 1] PlainCLIProtocol 프레임 인코딩/디코딩")
	fmt.Println(strings.Repeat("-", 70))
	fmt.Println()
	fmt.Println("프레임 구조 (PlainCLIProtocol.java):")
	fmt.Println()
	fmt.Println("  ┌──────────────┬──────────┬──────────────────────────┐")
	fmt.Println("  │ Length (4B)  │ Op (1B)  │ Data (Length bytes)      │")
	fmt.Println("  │ Big-endian   │ ordinal  │ Op에 따라 다름           │")
	fmt.Println("  └──────────────┴──────────┴──────────────────────────┘")
	fmt.Println()
	fmt.Println("Op 코드 목록:")
	fmt.Println("  ┌─────┬──────────┬─────────┬──────────────────────────┐")
	fmt.Println("  │ 값  │ 이름     │ 방향    │ 데이터                   │")
	fmt.Println("  ├─────┼──────────┼─────────┼──────────────────────────┤")
	fmt.Println("  │  0  │ ARG      │ C → S   │ writeUTF(명령/인자)      │")
	fmt.Println("  │  1  │ LOCALE   │ C → S   │ writeUTF(로케일)         │")
	fmt.Println("  │  2  │ ENCODING │ C → S   │ writeUTF(인코딩명)       │")
	fmt.Println("  │  3  │ START    │ C → S   │ (없음)                   │")
	fmt.Println("  │  4  │ EXIT     │ S → C   │ writeInt(종료코드)       │")
	fmt.Println("  │  5  │ STDIN    │ C → S   │ raw bytes                │")
	fmt.Println("  │  6  │ END_STIN │ C → S   │ (없음)                   │")
	fmt.Println("  │  7  │ STDOUT   │ S → C   │ raw bytes                │")
	fmt.Println("  │  8  │ STDERR   │ S → C   │ raw bytes                │")
	fmt.Println("  └─────┴──────────┴─────────┴──────────────────────────┘")
	fmt.Println()

	// 프레임 인코딩 예제
	testFrames := []Frame{
		{Op: OpARG, Data: encodeStringData("build")},
		{Op: OpARG, Data: encodeStringData("my-pipeline")},
		{Op: OpENCODING, Data: encodeStringData("UTF-8")},
		{Op: OpLOCALE, Data: encodeStringData("ko_KR")},
		{Op: OpSTART, Data: nil},
	}

	fmt.Println("클라이언트 → 서버 프레임 시퀀스 예제:")
	fmt.Println()
	for _, f := range testFrames {
		encoded := encodeFrame(f)
		length := binary.BigEndian.Uint32(encoded[:4])
		hexBytes := formatHex(encoded)
		dataStr := ""
		if f.Op == OpARG || f.Op == OpENCODING || f.Op == OpLOCALE {
			dataStr = fmt.Sprintf(" → \"%s\"", decodeStringData(f.Data))
		}
		fmt.Printf("  %-10s len=%-3d hex=[%s]%s\n",
			f.Op, length, hexBytes, dataStr)
	}

	// 디코딩 검증
	fmt.Println()
	fmt.Println("인코딩/디코딩 왕복 검증:")
	for _, f := range testFrames {
		encoded := encodeFrame(f)
		decoded, err := decodeFrame(bytes.NewReader(encoded))
		if err != nil {
			fmt.Printf("  ERROR: %v\n", err)
			continue
		}
		match := decoded.Op == f.Op && bytes.Equal(decoded.Data, f.Data)
		fmt.Printf("  %-10s: encode(%d bytes) → decode → Op일치=%v, Data일치=%v\n",
			f.Op, len(encoded), decoded.Op == f.Op, match)
	}

	// -----------------------------------------------------------------------
	// 데모 2: CLI 명령 레지스트리 및 디스패치
	// -----------------------------------------------------------------------
	fmt.Println()
	fmt.Println("[데모 2] CLICommand 레지스트리 및 디스패치")
	fmt.Println(strings.Repeat("-", 70))

	// 시뮬레이션용 작업 데이터
	jobs := map[string]*JobInfo{
		"my-pipeline":    {Name: "my-pipeline", Buildable: true},
		"deploy-staging": {Name: "deploy-staging", Buildable: true},
		"legacy-job":     {Name: "legacy-job", Buildable: false},
	}

	registry := NewCommandRegistry()
	registry.Register(&HelpCommand{registry: registry})
	registry.Register(&VersionCommand{})
	registry.Register(&WhoAmICommand{})
	registry.Register(&BuildCommand{jobs: jobs})
	registry.Register(&ListJobsCommand{jobs: jobs})
	registry.Register(&SafeRestartCommand{})

	fmt.Println()
	fmt.Println("등록된 명령 (ExtensionList<CLICommand> 시뮬레이션):")
	for _, cmd := range registry.All() {
		fmt.Printf("  %-20s — %s\n", cmd.GetName(), cmd.GetShortDescription())
	}

	// 명령 실행 테스트
	fmt.Println()
	fmt.Println("명령 디스패치 테스트:")

	adminAuth := &Authentication{Name: "admin", Authorities: []string{"authenticated", "admin"}}

	testCases := []struct {
		name string
		args []string
		auth *Authentication
	}{
		{"version", nil, nil},
		{"who-am-i (anonymous)", nil, nil},
		{"who-am-i (admin)", nil, adminAuth},
		{"list-jobs", nil, adminAuth},
		{"build my-pipeline", []string{"my-pipeline"}, adminAuth},
		{"build (no args)", nil, adminAuth},
		{"build legacy-job", []string{"legacy-job"}, adminAuth},
		{"build (no auth)", []string{"my-pipeline"}, nil},
	}

	for _, tc := range testCases {
		cmdName := tc.name
		if idx := strings.IndexByte(cmdName, ' '); idx > 0 {
			cmdName = cmdName[:idx]
		}
		if idx := strings.IndexByte(cmdName, ' '); idx > 0 {
			cmdName = cmdName[:idx]
		}
		// 명령명에서 괄호 제거
		cmdName = strings.Split(cmdName, " ")[0]

		cmd := registry.Clone(cmdName)
		if cmd == nil {
			fmt.Printf("\n  [%s] → ERROR: No such command\n", tc.name)
			continue
		}

		exitCode, stdout, stderr := cmd.Run(tc.args, tc.auth)
		fmt.Printf("\n  [%s] → exit=%d\n", tc.name, exitCode)
		if stdout != "" {
			for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
				fmt.Printf("    stdout: %s\n", line)
			}
		}
		if stderr != "" {
			for _, line := range strings.Split(strings.TrimRight(stderr, "\n"), "\n") {
				fmt.Printf("    stderr: %s\n", line)
			}
		}
	}

	// -----------------------------------------------------------------------
	// 데모 3: TCP 소켓 위 PlainCLIProtocol 전체 흐름
	// -----------------------------------------------------------------------
	fmt.Println()
	fmt.Println("[데모 3] TCP 소켓 위 PlainCLIProtocol 전체 흐름")
	fmt.Println(strings.Repeat("-", 70))
	fmt.Println()
	fmt.Println("통신 시퀀스 (CLIAction.ServerSideImpl / CLI.ClientSideImpl):")
	fmt.Println()
	fmt.Println("  Client (jenkins-cli.jar)          Server (CLIAction)")
	fmt.Println("  ┌─────────────────────┐           ┌─────────────────────┐")
	fmt.Println("  │ ClientSideImpl      │           │ ServerSideImpl      │")
	fmt.Println("  │                     │  ARG(cmd) │                     │")
	fmt.Println("  │ sendArg(\"build\")    ├──────────►│ onArg(\"build\")      │")
	fmt.Println("  │ sendArg(\"my-job\")   ├──────────►│ onArg(\"my-job\")     │")
	fmt.Println("  │ sendEncoding(\"UTF-8\")├─────────►│ onEncoding(\"UTF-8\") │")
	fmt.Println("  │ sendLocale(\"ko_KR\") ├──────────►│ onLocale(\"ko_KR\")   │")
	fmt.Println("  │ sendStart()         ├──────────►│ onStart() → ready() │")
	fmt.Println("  │                     │           │                     │")
	fmt.Println("  │                     │           │ CLICommand.clone()  │")
	fmt.Println("  │                     │           │ command.main(args)  │")
	fmt.Println("  │                     │           │                     │")
	fmt.Println("  │                     │  STDOUT   │                     │")
	fmt.Println("  │ onStdout(chunk)     │◄──────────┤ streamStdout()      │")
	fmt.Println("  │                     │  STDERR   │                     │")
	fmt.Println("  │ onStderr(chunk)     │◄──────────┤ streamStderr()      │")
	fmt.Println("  │                     │  EXIT     │                     │")
	fmt.Println("  │ onExit(code)        │◄──────────┤ sendExit(code)      │")
	fmt.Println("  │ finished()          │           │                     │")
	fmt.Println("  └─────────────────────┘           └─────────────────────┘")
	fmt.Println()

	// 서버 시작
	server, err := NewCLIServer("127.0.0.1:0", registry)
	if err != nil {
		fmt.Printf("서버 시작 실패: %v\n", err)
		return
	}
	go server.Serve()
	defer server.Close()
	fmt.Printf("CLI 서버 시작: %s\n\n", server.Addr())

	// 클라이언트 요청 시퀀스
	clientTests := []struct {
		desc string
		args []string
	}{
		{"help 명령 (전체 명령 목록)", []string{"help"}},
		{"version 명령", []string{"version"}},
		{"who-am-i (인증 포함)", []string{"who-am-i", "-auth", "admin:token123"}},
		{"list-jobs 명령", []string{"list-jobs"}},
		{"build my-pipeline (인증 포함)", []string{"build", "my-pipeline", "-auth", "admin:token123"}},
		{"존재하지 않는 명령", []string{"unknown-cmd"}},
	}

	for _, tc := range clientTests {
		fmt.Printf("--- %s ---\n", tc.desc)
		fmt.Printf("  요청: java -jar jenkins-cli.jar %s\n", strings.Join(tc.args, " "))

		client, err := NewCLIClient(server.Addr())
		if err != nil {
			fmt.Printf("  연결 실패: %v\n\n", err)
			continue
		}

		exitCode, stdout, stderr := client.Execute(tc.args)
		fmt.Printf("  종료 코드: %d\n", exitCode)
		if stdout != "" {
			for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
				fmt.Printf("  stdout: %s\n", line)
			}
		}
		if stderr != "" {
			for _, line := range strings.Split(strings.TrimRight(stderr, "\n"), "\n") {
				fmt.Printf("  stderr: %s\n", line)
			}
		}
		fmt.Println()
	}

	// -----------------------------------------------------------------------
	// 데모 4: Remoting Channel RPC (간략)
	// -----------------------------------------------------------------------
	fmt.Println("[데모 4] Remoting Channel RPC 시뮬레이션 (간략)")
	fmt.Println(strings.Repeat("-", 70))
	fmt.Println()
	fmt.Println("Remoting Channel 구조:")
	fmt.Println()
	fmt.Println("  Agent                              Controller")
	fmt.Println("  ┌────────────────┐                ┌────────────────┐")
	fmt.Println("  │                │  Request(ID=1) │                │")
	fmt.Println("  │ channel.call() ├───────────────►│ handler(args)  │")
	fmt.Println("  │                │                │                │")
	fmt.Println("  │                │  Response(ID=1)│                │")
	fmt.Println("  │ pendingCalls[1]│◄───────────────┤ result/error   │")
	fmt.Println("  └────────────────┘                └────────────────┘")
	fmt.Println()

	ch := NewRemotingChannel()

	// 핸들러 등록
	ch.RegisterHandler("getSystemProperty", func(args []string) (string, error) {
		props := map[string]string{
			"os.name":    "Linux",
			"os.version": "5.15.0",
			"user.home":  "/var/jenkins_home",
		}
		if len(args) > 0 {
			if v, ok := props[args[0]]; ok {
				return v, nil
			}
			return "", fmt.Errorf("property not found: %s", args[0])
		}
		return "", fmt.Errorf("property name required")
	})

	ch.RegisterHandler("getEnvironmentVariable", func(args []string) (string, error) {
		envs := map[string]string{
			"JENKINS_HOME": "/var/jenkins_home",
			"JAVA_HOME":    "/usr/lib/jvm/java-17",
			"PATH":         "/usr/local/bin:/usr/bin:/bin",
		}
		if len(args) > 0 {
			if v, ok := envs[args[0]]; ok {
				return v, nil
			}
			return "", fmt.Errorf("env not found: %s", args[0])
		}
		return "", fmt.Errorf("env name required")
	})

	// RPC 호출 테스트
	rpcTests := []struct {
		method string
		args   []string
	}{
		{"getSystemProperty", []string{"os.name"}},
		{"getSystemProperty", []string{"user.home"}},
		{"getEnvironmentVariable", []string{"JENKINS_HOME"}},
		{"getEnvironmentVariable", []string{"JAVA_HOME"}},
		{"getSystemProperty", []string{"nonexistent"}},
	}

	for _, tc := range rpcTests {
		resp := ch.Call(tc.method, tc.args)
		if resp.Error != "" {
			fmt.Printf("  %s(%s) → ERROR: %s\n", tc.method, strings.Join(tc.args, ","), resp.Error)
		} else {
			fmt.Printf("  %s(%s) → \"%s\"\n", tc.method, strings.Join(tc.args, ","), resp.Result)
		}
	}

	// -----------------------------------------------------------------------
	// 데모 5: CLICommand.getName() 변환 규칙
	// -----------------------------------------------------------------------
	fmt.Println()
	fmt.Println("[데모 5] CLICommand.getName() — 클래스명 → 명령명 변환 규칙")
	fmt.Println(strings.Repeat("-", 70))
	fmt.Println()
	fmt.Println("CLICommand.java 178~188행:")
	fmt.Println("  name = name.replaceAll(\"([a-z0-9])([A-Z])\", \"$1-$2\").toLowerCase()")
	fmt.Println()

	classNames := []string{
		"BuildCommand",
		"ListJobsCommand",
		"WhoAmICommand",
		"SafeRestartCommand",
		"CreateNodeCommand",
		"GetJobCommand",
		"DeleteBuildsCommand",
		"InstallPluginCommand",
		"ReloadConfigurationCommand",
	}

	fmt.Println("  ┌──────────────────────────────┬──────────────────────────────┐")
	fmt.Println("  │ 클래스명                     │ getName() 결과              │")
	fmt.Println("  ├──────────────────────────────┼──────────────────────────────┤")
	for _, className := range classNames {
		name := convertToCommandName(className)
		fmt.Printf("  │ %-28s │ %-28s │\n", className, name)
	}
	fmt.Println("  └──────────────────────────────┴──────────────────────────────┘")

	// -----------------------------------------------------------------------
	// 데모 6: CLICommand 종료 코드 매핑
	// -----------------------------------------------------------------------
	fmt.Println()
	fmt.Println("[데모 6] CLICommand 종료 코드 매핑")
	fmt.Println(strings.Repeat("-", 70))
	fmt.Println()
	fmt.Println("CLICommand.java 222~234행의 종료 코드 정의:")
	fmt.Println()
	fmt.Println("  ┌──────┬──────────────────────────────────────────────────────┐")
	fmt.Println("  │ 코드 │ 의미                                               │")
	fmt.Println("  ├──────┼──────────────────────────────────────────────────────┤")
	fmt.Println("  │  0   │ 정상 완료                                           │")
	fmt.Println("  │  1   │ 예기치 않은 예외                                     │")
	fmt.Println("  │  2   │ CmdLineException (잘못된 인자)                       │")
	fmt.Println("  │  3   │ IllegalArgumentException (잘못된 입력값)             │")
	fmt.Println("  │  4   │ IllegalStateException (잘못된 상태)                  │")
	fmt.Println("  │  5   │ AbortException (예견된 중단)                         │")
	fmt.Println("  │  6   │ AccessDeniedException (권한 부족)                    │")
	fmt.Println("  │  7   │ BadCredentialsException (잘못된 자격 증명)           │")
	fmt.Println("  │ 8-15 │ 예약됨 (향후 사용)                                  │")
	fmt.Println("  │ 16+  │ 명령별 커스텀 종료 코드                              │")
	fmt.Println("  └──────┴──────────────────────────────────────────────────────┘")

	fmt.Println()
	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Println("  시뮬레이션 완료")
	fmt.Println("=" + strings.Repeat("=", 79))
}

// convertToCommandName은 Java 클래스명을 Jenkins CLI 명령명으로 변환한다.
// CLICommand.java 178~188행:
//   name = name.substring(name.lastIndexOf('.') + 1);  // 패키지 제거
//   name = name.substring(name.lastIndexOf('$') + 1);  // 내부 클래스 처리
//   if (name.endsWith("Command"))
//       name = name.substring(0, name.length() - 7);   // "Command" 접미사 제거
//   return name.replaceAll("([a-z0-9])([A-Z])", "$1-$2").toLowerCase(Locale.ENGLISH);
func convertToCommandName(className string) string {
	// 패키지명 제거
	if idx := strings.LastIndex(className, "."); idx >= 0 {
		className = className[idx+1:]
	}
	// 내부 클래스 처리
	if idx := strings.LastIndex(className, "$"); idx >= 0 {
		className = className[idx+1:]
	}
	// "Command" 접미사 제거
	className = strings.TrimSuffix(className, "Command")

	// CamelCase → kebab-case
	// "([a-z0-9])([A-Z])" → "$1-$2"
	var result strings.Builder
	for i, ch := range className {
		if i > 0 && ch >= 'A' && ch <= 'Z' {
			prev := className[i-1]
			if (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9') {
				result.WriteByte('-')
			}
		}
		result.WriteRune(ch)
	}
	return strings.ToLower(result.String())
}

// formatHex는 바이트 슬라이스를 공백 구분 16진수 문자열로 변환한다.
func formatHex(data []byte) string {
	parts := make([]string, len(data))
	for i, b := range data {
		parts[i] = fmt.Sprintf("%02x", b)
	}
	return strings.Join(parts, " ")
}
