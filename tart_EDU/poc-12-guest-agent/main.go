package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// tart Guest Agent 명령 실행 시뮬레이션
// =============================================================================
//
// tart exec 명령은 실행 중인 VM 내부에서 명령을 실행한다.
// VM 디렉토리의 control.sock (Unix 소켓)을 통해 Guest Agent와 통신한다.
//
// 실제 흐름:
// 1. 호스트: VM 디렉토리 열기 → control.sock으로 gRPC 연결
// 2. 호스트: ExecCommand 메시지 전송 (command, args, interactive, tty)
// 3. 게스트: 명령 실행 → stdout/stderr 스트리밍 → exit code 반환
// 4. 동시 스트림: stdin/stdout/stderr goroutine으로 병렬 처리
//
// 실제 소스: Sources/tart/Commands/Exec.swift

// =============================================================================
// 프로토콜 메시지 타입 — tart의 gRPC Exec 메시지를 JSON으로 시뮬레이션
// =============================================================================

// MessageType은 프로토콜 메시지 유형을 나타낸다.
type MessageType string

const (
	// 호스트 → 게스트 메시지
	MsgCommand       MessageType = "command"        // 명령 실행 요청
	MsgStandardInput MessageType = "standard_input"  // stdin 데이터 전송
	MsgTermResize    MessageType = "terminal_resize"  // 터미널 크기 변경

	// 게스트 → 호스트 메시지
	MsgStandardOutput MessageType = "standard_output" // stdout 데이터
	MsgStandardError  MessageType = "standard_error"   // stderr 데이터
	MsgExit           MessageType = "exit"             // 종료 코드
)

// ExecRequest는 호스트에서 게스트로 보내는 요청 메시지이다.
// tart의 execCall.requestStream.send()에 대응한다.
type ExecRequest struct {
	Type MessageType `json:"type"`

	// command 타입일 때
	Name        string   `json:"name,omitempty"`        // 실행할 명령
	Args        []string `json:"args,omitempty"`        // 명령 인자
	Interactive bool     `json:"interactive,omitempty"` // -i 플래그
	TTY         bool     `json:"tty,omitempty"`         // -t 플래그

	// standard_input 타입일 때
	Data string `json:"data,omitempty"` // stdin 데이터

	// terminal_resize 타입일 때
	Cols int `json:"cols,omitempty"` // 터미널 열 수
	Rows int `json:"rows,omitempty"` // 터미널 행 수
}

// ExecResponse는 게스트에서 호스트로 보내는 응답 메시지이다.
// tart의 execCall.responseStream에 대응한다.
type ExecResponse struct {
	Type MessageType `json:"type"`
	Data string      `json:"data,omitempty"` // stdout/stderr 데이터
	Code int         `json:"code,omitempty"` // 종료 코드
}

// =============================================================================
// Guest Agent 서버 — Unix 소켓에서 명령을 수신하고 실행
// =============================================================================

// GuestAgent는 VM 내부에서 실행되는 게스트 에이전트를 시뮬레이션한다.
// tart에서는 Tart Guest Agent가 control.sock에서 gRPC 서버로 동작한다.
type GuestAgent struct {
	socketPath string
	listener   net.Listener
}

// NewGuestAgent는 Unix 소켓에서 리스닝하는 게스트 에이전트를 생성한다.
// tart에서는 VM 디렉토리 내 control.sock 파일을 사용한다.
// Unix 소켓 경로 104바이트 제한 때문에 tart는 baseURL로 cwd를 변경한다.
// (https://blog.8-p.info/en/2020/06/11/unix-domain-socket-length/)
func NewGuestAgent(socketPath string) (*GuestAgent, error) {
	// 기존 소켓 파일 제거
	os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("Unix 소켓 리스닝 실패: %w", err)
	}

	return &GuestAgent{
		socketPath: socketPath,
		listener:   listener,
	}, nil
}

// Serve는 클라이언트 연결을 수락하고 명령을 처리한다.
func (ga *GuestAgent) Serve(ready chan<- struct{}) {
	fmt.Printf("[게스트] 에이전트 시작: %s\n", ga.socketPath)
	close(ready)

	for {
		conn, err := ga.listener.Accept()
		if err != nil {
			return // 리스너 종료
		}
		go ga.handleConnection(conn)
	}
}

// handleConnection은 개별 클라이언트 연결을 처리한다.
// tart의 AgentAsyncClient.makeExecCall()에 대응한다.
func (ga *GuestAgent) handleConnection(conn net.Conn) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	encoder := json.NewEncoder(conn)

	for {
		// 줄 단위로 JSON 메시지 읽기
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}

		var req ExecRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}

		switch req.Type {
		case MsgCommand:
			ga.executeCommand(req, encoder, reader)
			return // 명령 실행 후 연결 종료

		case MsgStandardInput:
			// 대화형 모드에서 stdin 데이터 수신 (여기서는 로그만)
			fmt.Printf("[게스트] stdin 수신: %q\n", req.Data)
		}
	}
}

// executeCommand는 명령을 실행하고 결과를 스트리밍한다.
// tart의 execute() 함수에 대응한다.
//
// tart 원본 흐름:
//
//	1. execCall.requestStream.send(.command(...))
//	2. 게스트에서 명령 실행
//	3. response 스트림으로 stdout/stderr/exit 전송
//	4. 호스트에서 FileHandle.standardOutput/standardError에 출력
func (ga *GuestAgent) executeCommand(req ExecRequest, encoder *json.Encoder, reader *bufio.Reader) {
	fmt.Printf("[게스트] 명령 수신: %s %s (interactive=%v, tty=%v)\n",
		req.Name, strings.Join(req.Args, " "), req.Interactive, req.TTY)

	// 명령 실행
	cmd := exec.Command(req.Name, req.Args...)

	// stdout/stderr 파이프 설정
	stdoutPipe, _ := cmd.StdoutPipe()
	stderrPipe, _ := cmd.StderrPipe()

	// 대화형 모드면 stdin도 연결
	var stdinPipe io.WriteCloser
	if req.Interactive {
		stdinPipe, _ = cmd.StdinPipe()
		// stdin 전달 goroutine
		go func() {
			defer stdinPipe.Close()
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				var stdinReq ExecRequest
				if err := json.Unmarshal([]byte(line), &stdinReq); err != nil {
					continue
				}
				if stdinReq.Type == MsgStandardInput {
					if stdinReq.Data == "" {
						// EOF 시그널 — tart에서 빈 Data로 EOF를 전달
						return
					}
					stdinPipe.Write([]byte(stdinReq.Data))
				}
			}
		}()
	}

	if err := cmd.Start(); err != nil {
		// 명령 실행 실패
		encoder.Encode(ExecResponse{
			Type: MsgStandardError,
			Data: fmt.Sprintf("명령 실행 실패: %s\n", err),
		})
		encoder.Encode(ExecResponse{Type: MsgExit, Code: 127})
		return
	}

	// stdout/stderr를 동시에 스트리밍
	// tart에서는 withThrowingTaskGroup으로 병렬 처리
	var wg sync.WaitGroup

	// stdout 스트리밍
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := stdoutPipe.Read(buf)
			if n > 0 {
				encoder.Encode(ExecResponse{
					Type: MsgStandardOutput,
					Data: string(buf[:n]),
				})
			}
			if err != nil {
				return
			}
		}
	}()

	// stderr 스트리밍
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := stderrPipe.Read(buf)
			if n > 0 {
				encoder.Encode(ExecResponse{
					Type: MsgStandardError,
					Data: string(buf[:n]),
				})
			}
			if err != nil {
				return
			}
		}
	}()

	wg.Wait()

	// 종료 코드
	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	encoder.Encode(ExecResponse{Type: MsgExit, Code: exitCode})
	fmt.Printf("[게스트] 명령 종료: exit_code=%d\n", exitCode)
}

// Close는 게스트 에이전트를 종료한다.
func (ga *GuestAgent) Close() {
	ga.listener.Close()
	os.Remove(ga.socketPath)
}

// =============================================================================
// 호스트 클라이언트 — Unix 소켓을 통해 게스트에 명령 전송
// =============================================================================

// HostClient는 호스트에서 게스트 에이전트에 연결하는 클라이언트이다.
// tart exec 명령의 호스트 측 로직을 시뮬레이션한다.
type HostClient struct {
	conn    net.Conn
	encoder *json.Encoder
	reader  *bufio.Reader
}

// ConnectToGuest는 게스트 에이전트의 Unix 소켓에 연결한다.
// tart에서는 GRPCChannelPool.with(target: .unixDomainSocket(...))로 연결한다.
func ConnectToGuest(socketPath string) (*HostClient, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("게스트 연결 실패: %w", err)
	}

	return &HostClient{
		conn:    conn,
		encoder: json.NewEncoder(conn),
		reader:  bufio.NewReader(conn),
	}, nil
}

// Execute는 게스트에서 명령을 실행하고 결과를 수신한다.
// tart Exec.execute() 함수의 호스트 측 로직을 시뮬레이션한다.
func (hc *HostClient) Execute(name string, args []string, interactive bool) (int, error) {
	// 1. 명령 전송
	req := ExecRequest{
		Type:        MsgCommand,
		Name:        name,
		Args:        args,
		Interactive: interactive,
	}

	data, _ := json.Marshal(req)
	hc.conn.Write(append(data, '\n'))

	fmt.Printf("[호스트] 명령 전송: %s %s\n", name, strings.Join(args, " "))

	// 2. 응답 스트리밍 수신
	// tart에서는 for try await response in execCall.responseStream 으로 처리
	for {
		line, err := hc.reader.ReadString('\n')
		if err != nil {
			return 1, fmt.Errorf("응답 읽기 실패: %w", err)
		}

		var resp ExecResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			continue
		}

		switch resp.Type {
		case MsgStandardOutput:
			// tart: try FileHandle.standardOutput.write(contentsOf: ioChunk.data)
			fmt.Printf("[호스트:stdout] %s", resp.Data)
		case MsgStandardError:
			// tart: try FileHandle.standardError.write(contentsOf: ioChunk.data)
			fmt.Fprintf(os.Stderr, "[호스트:stderr] %s", resp.Data)
		case MsgExit:
			// tart: throw ExecCustomExitCodeError(exitCode: exit.code)
			fmt.Printf("[호스트] 종료 코드 수신: %d\n", resp.Code)
			return resp.Code, nil
		}
	}
}

// SendStdin은 대화형 모드에서 stdin 데이터를 게스트에 전송한다.
// tart에서는 FileHandle.standardInput의 readabilityHandler를 통해 전달한다.
func (hc *HostClient) SendStdin(data string) {
	req := ExecRequest{
		Type: MsgStandardInput,
		Data: data,
	}
	d, _ := json.Marshal(req)
	hc.conn.Write(append(d, '\n'))
}

// Close는 호스트 클라이언트를 종료한다.
func (hc *HostClient) Close() {
	hc.conn.Close()
}

// =============================================================================
// 데모 헬퍼
// =============================================================================

func printSeparator(title string) {
	fmt.Printf("\n%s\n", strings.Repeat("=", 70))
	fmt.Printf("  %s\n", title)
	fmt.Printf("%s\n\n", strings.Repeat("=", 70))
}

// =============================================================================
// 메인 함수
// =============================================================================

func main() {
	fmt.Println("tart Guest Agent 명령 실행 시뮬레이션")
	fmt.Println("실제 소스: Sources/tart/Commands/Exec.swift")
	fmt.Println()

	// 임시 디렉토리에 소켓 파일 생성
	tmpDir, err := os.MkdirTemp("", "tart-poc-12-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "임시 디렉토리 생성 실패: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)

	socketPath := filepath.Join(tmpDir, "control.sock")

	// =========================================================================
	// 1. 기본 명령 실행 (비대화형)
	// =========================================================================
	printSeparator("1. 기본 명령 실행 (echo)")

	agent, err := NewGuestAgent(socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "게스트 에이전트 생성 실패: %v\n", err)
		os.Exit(1)
	}

	ready := make(chan struct{})
	go agent.Serve(ready)
	<-ready

	client, err := ConnectToGuest(socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "연결 실패: %v\n", err)
		os.Exit(1)
	}

	exitCode, err := client.Execute("echo", []string{"Hello from tart VM!"}, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "실행 오류: %v\n", err)
	}
	fmt.Printf("최종 종료 코드: %d\n", exitCode)
	client.Close()
	agent.Close()

	// =========================================================================
	// 2. 다중 줄 출력 명령
	// =========================================================================
	printSeparator("2. 다중 줄 출력 명령 (ls -la)")

	agent, _ = NewGuestAgent(socketPath)
	ready = make(chan struct{})
	go agent.Serve(ready)
	<-ready

	client, _ = ConnectToGuest(socketPath)
	exitCode, _ = client.Execute("ls", []string{"-la", tmpDir}, false)
	fmt.Printf("최종 종료 코드: %d\n", exitCode)
	client.Close()
	agent.Close()

	// =========================================================================
	// 3. stderr 출력 명령
	// =========================================================================
	printSeparator("3. stderr 출력 (존재하지 않는 경로)")

	agent, _ = NewGuestAgent(socketPath)
	ready = make(chan struct{})
	go agent.Serve(ready)
	<-ready

	client, _ = ConnectToGuest(socketPath)
	exitCode, _ = client.Execute("ls", []string{"/nonexistent-path-tart-poc"}, false)
	fmt.Printf("최종 종료 코드: %d (0이 아님 = 에러)\n", exitCode)
	client.Close()
	agent.Close()

	// =========================================================================
	// 4. 존재하지 않는 명령 실행
	// =========================================================================
	printSeparator("4. 존재하지 않는 명령 실행")

	agent, _ = NewGuestAgent(socketPath)
	ready = make(chan struct{})
	go agent.Serve(ready)
	<-ready

	client, _ = ConnectToGuest(socketPath)
	exitCode, _ = client.Execute("nonexistent-command-xyz", []string{}, false)
	fmt.Printf("최종 종료 코드: %d (127 = 명령 없음)\n", exitCode)
	client.Close()
	agent.Close()

	// =========================================================================
	// 5. 대화형 모드 (stdin 전달)
	// =========================================================================
	printSeparator("5. 대화형 모드 — stdin → 게스트 전달")

	agent, _ = NewGuestAgent(socketPath)
	ready = make(chan struct{})
	go agent.Serve(ready)
	<-ready

	client, _ = ConnectToGuest(socketPath)

	// 대화형 명령: cat은 stdin을 stdout으로 에코
	// stdin을 별도 goroutine에서 전송
	go func() {
		time.Sleep(100 * time.Millisecond)
		client.SendStdin("첫 번째 줄\n")
		time.Sleep(50 * time.Millisecond)
		client.SendStdin("두 번째 줄\n")
		time.Sleep(50 * time.Millisecond)
		client.SendStdin("") // EOF 전송
	}()

	exitCode, _ = client.Execute("cat", []string{}, true)
	fmt.Printf("최종 종료 코드: %d\n", exitCode)
	client.Close()
	agent.Close()

	// =========================================================================
	// 6. 동시 명령 실행 (여러 클라이언트)
	// =========================================================================
	printSeparator("6. 동시 명령 실행 (3개 클라이언트)")

	agent, _ = NewGuestAgent(socketPath)
	ready = make(chan struct{})
	go agent.Serve(ready)
	<-ready

	var wg sync.WaitGroup
	commands := []struct {
		name string
		args []string
	}{
		{"echo", []string{"[클라이언트1] Hello"}},
		{"echo", []string{"[클라이언트2] World"}},
		{"echo", []string{"[클라이언트3] tart!"}},
	}

	for i, cmd := range commands {
		wg.Add(1)
		go func(idx int, name string, args []string) {
			defer wg.Done()
			c, err := ConnectToGuest(socketPath)
			if err != nil {
				fmt.Printf("  클라이언트%d 연결 실패: %v\n", idx+1, err)
				return
			}
			defer c.Close()

			code, _ := c.Execute(name, args, false)
			fmt.Printf("  클라이언트%d 종료: exit=%d\n", idx+1, code)
		}(i, cmd.name, cmd.args)
	}

	wg.Wait()
	agent.Close()

	// =========================================================================
	// 7. 프로토콜 메시지 구조 설명
	// =========================================================================
	printSeparator("7. 프로토콜 메시지 구조")

	fmt.Println("  호스트 → 게스트 메시지:")
	msgs := []ExecRequest{
		{Type: MsgCommand, Name: "ls", Args: []string{"-la"}, Interactive: false, TTY: false},
		{Type: MsgStandardInput, Data: "입력 데이터"},
		{Type: MsgTermResize, Cols: 120, Rows: 40},
	}
	for _, msg := range msgs {
		data, _ := json.MarshalIndent(msg, "    ", "  ")
		fmt.Printf("    %s\n", data)
	}

	fmt.Println("\n  게스트 → 호스트 메시지:")
	resps := []ExecResponse{
		{Type: MsgStandardOutput, Data: "출력 데이터\n"},
		{Type: MsgStandardError, Data: "에러 메시지\n"},
		{Type: MsgExit, Code: 0},
	}
	for _, resp := range resps {
		data, _ := json.MarshalIndent(resp, "    ", "  ")
		fmt.Printf("    %s\n", data)
	}

	fmt.Println("\n[완료] tart Guest Agent 시뮬레이션 종료")
}
