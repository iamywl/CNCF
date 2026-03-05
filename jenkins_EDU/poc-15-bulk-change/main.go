// poc-15-bulk-change: Jenkins BulkChange 저장 트랜잭션 패턴 시뮬레이션
//
// Jenkins에서 Saveable 객체의 설정을 여러 번 변경하면, 매번 save()가 호출되어
// 디스크 I/O가 발생한다. BulkChange는 이 문제를 해결하는 트랜잭션 패턴으로,
// 여러 변경을 하나의 save() 호출로 묶어준다.
//
// 참조 소스 코드:
//   - jenkins/core/src/main/java/hudson/BulkChange.java
//     : ThreadLocal 기반 스택 구조, commit()/abort()/close() 패턴
//     : contains(Saveable) — 현재 스레드의 BulkChange 체인에서 해당 Saveable 포함 여부 확인
//     : ALL 매직 인스턴스 — 모든 save()를 억제하는 특수 Saveable
//   - jenkins/core/src/main/java/hudson/model/Saveable.java
//     : save() 인터페이스 — XML 영속 대상 객체의 공통 계약
//   - jenkins/core/src/main/java/hudson/util/AtomicFileWriter.java
//     : 임시 파일에 쓰기 → commit() 시 rename으로 원자적 교체, abort() 시 삭제
//   - jenkins/core/src/main/java/hudson/model/listeners/SaveableListener.java
//     : fireOnChange(Saveable, XmlFile) — 저장 완료 후 리스너 콜백
//   - jenkins/core/src/main/java/jenkins/model/Jenkins.java (3579행)
//     : if (BulkChange.contains(this)) { return; } — BulkChange 활성 시 저장 억제
//
// 실행: go run main.go

package main

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 1. Saveable 인터페이스 — jenkins/core/src/main/java/hudson/model/Saveable.java
// ============================================================================
// Jenkins에서 XML로 영속되는 모든 객체는 Saveable 인터페이스를 구현한다.
// save() 구현 내부에서 BulkChange.contains(this)를 확인하고,
// true이면 실제 저장을 억제한다. 이것이 BulkChange 패턴의 핵심 협력 규약이다.
//
//   public interface Saveable {
//       void save() throws IOException;
//       Saveable NOOP = () -> {};  // 아무것도 하지 않는 Saveable
//   }

type Saveable interface {
	Save() error
	Name() string
}

// ============================================================================
// 2. AtomicFileWriter — jenkins/core/src/main/java/hudson/util/AtomicFileWriter.java
// ============================================================================
// 원자적 파일 쓰기: 임시 파일에 먼저 기록 → commit()에서 rename으로 교체.
// 실제 구현에서는 FileChannelWriter를 내부적으로 사용하며,
// Cleaner를 등록하여 GC 시 열린 파일을 자동 정리한다.
//
// 핵심 메서드:
//   - write(): 임시 파일(tmpPath)에 데이터 기록
//   - commit(): close() → Files.move(tmpPath, destPath, ATOMIC_MOVE) → 디렉토리 fsync
//   - abort(): close() → Files.deleteIfExists(tmpPath)
//
// 이중 안전장치: atomicMoveSupported 플래그로 ATOMIC_MOVE 실패 시 REPLACE_EXISTING 폴백

type AtomicFileWriter struct {
	destPath string // 최종 대상 파일 경로
	tmpPath  string // 임시 파일 경로
	tmpFile  *os.File
	closed   bool
}

// NewAtomicFileWriter: 임시 파일을 생성하고 writer를 반환한다.
// 실제 Jenkins에서는 destPath.getFileName() + "-atomic" + "tmp" 패턴으로 임시 파일을 만든다.
func NewAtomicFileWriter(destPath string) (*AtomicFileWriter, error) {
	dir := filepath.Dir(destPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("디렉토리 생성 실패 %s: %w", dir, err)
	}

	base := filepath.Base(destPath)
	tmpFile, err := os.CreateTemp(dir, base+"-atomic*.tmp")
	if err != nil {
		return nil, fmt.Errorf("임시 파일 생성 실패: %w", err)
	}

	return &AtomicFileWriter{
		destPath: destPath,
		tmpPath:  tmpFile.Name(),
		tmpFile:  tmpFile,
	}, nil
}

// Write: 임시 파일에 데이터를 기록한다.
func (w *AtomicFileWriter) Write(data []byte) (int, error) {
	if w.closed {
		return 0, fmt.Errorf("AtomicFileWriter가 이미 닫힘")
	}
	return w.tmpFile.Write(data)
}

// Commit: 임시 파일을 닫고 rename으로 원자적으로 교체한다.
// 실제 Jenkins 구현:
//   public void commit() throws IOException {
//       close();
//       move(tmpPath, destPath);  // ATOMIC_MOVE 시도, 실패 시 REPLACE_EXISTING 폴백
//       // 디렉토리 fsync (Linux에서 필요)
//   }
func (w *AtomicFileWriter) Commit() error {
	if err := w.close(); err != nil {
		return err
	}
	// os.Rename은 같은 파일시스템 내에서 원자적이다 (POSIX)
	if err := os.Rename(w.tmpPath, w.destPath); err != nil {
		// 원자적 이동 실패 시 정리
		os.Remove(w.tmpPath)
		return fmt.Errorf("원자적 이동 실패 %s → %s: %w", w.tmpPath, w.destPath, err)
	}
	return nil
}

// Abort: 임시 파일을 삭제하고 원본을 보존한다.
// 실제 Jenkins 구현:
//   public void abort() throws IOException {
//       try { close(); } finally { Files.deleteIfExists(tmpPath); }
//   }
func (w *AtomicFileWriter) Abort() error {
	w.close()
	return os.Remove(w.tmpPath)
}

func (w *AtomicFileWriter) close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	return w.tmpFile.Close()
}

// ============================================================================
// 3. SaveableListener — jenkins/core/.../listeners/SaveableListener.java
// ============================================================================
// Saveable 객체가 저장될 때 등록된 리스너에게 알린다.
// 실제 Jenkins에서는 ExtensionPoint로 자동 등록되며,
// fireOnChange() 정적 메서드로 모든 리스너에게 통지한다.
//
//   public static void fireOnChange(Saveable o, XmlFile file) {
//       Listeners.notify(SaveableListener.class, false, l -> l.onChange(o, file));
//   }

type SaveableListener interface {
	OnChange(s Saveable, filePath string)
}

type SaveableListenerRegistry struct {
	mu        sync.RWMutex
	listeners []SaveableListener
}

var globalListenerRegistry = &SaveableListenerRegistry{}

func (r *SaveableListenerRegistry) Register(l SaveableListener) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listeners = append(r.listeners, l)
}

// FireOnChange: 모든 리스너에게 저장 이벤트를 통지한다.
func (r *SaveableListenerRegistry) FireOnChange(s Saveable, filePath string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, l := range r.listeners {
		l.OnChange(s, filePath)
	}
}

// LoggingSaveableListener: 저장 이벤트를 로깅하는 리스너 구현
type LoggingSaveableListener struct {
	events []string
}

func (l *LoggingSaveableListener) OnChange(s Saveable, filePath string) {
	msg := fmt.Sprintf("  [SaveableListener] onChange: %s → %s", s.Name(), filePath)
	l.events = append(l.events, msg)
	fmt.Println(msg)
}

// ============================================================================
// 4. BulkChange — jenkins/core/src/main/java/hudson/BulkChange.java
// ============================================================================
// 트랜잭션 패턴: 여러 설정 변경을 하나의 save()로 묶는다.
//
// 핵심 구조:
//   - ThreadLocal<BulkChange> INSCOPE: 현재 스레드의 활성 BulkChange (스택)
//   - parent: 중첩 BulkChange를 위한 링크드 리스트
//   - saveable: 이 BulkChange가 관리하는 Saveable 객체
//   - completed: commit() 또는 abort() 호출 여부
//
// 실제 Java 코드:
//   public class BulkChange implements Closeable {
//       private final Saveable saveable;
//       private final BulkChange parent;
//       private boolean completed;
//       private static final ThreadLocal<BulkChange> INSCOPE = new ThreadLocal<>();
//
//       public BulkChange(Saveable saveable) {
//           this.parent = current();   // 이전 BulkChange를 parent로 저장
//           this.saveable = saveable;
//           INSCOPE.set(this);         // 자신을 현재 스코프로 설정
//       }
//   }
//
// Go에서는 ThreadLocal 대신 goroutine ID 기반 맵을 사용한다.
// (실제 프로덕션에서는 context.Context를 사용하는 것이 관용적이지만,
//  Jenkins의 ThreadLocal 패턴을 충실하게 재현하기 위해 goroutine-local 방식 사용)

// goroutineLocal: Go에서 Java의 ThreadLocal을 시뮬레이션
// 실제 Jenkins는 ThreadLocal<BulkChange> INSCOPE를 사용한다.
type goroutineLocal struct {
	mu    sync.RWMutex
	store map[int64]*BulkChange // goroutine ID → BulkChange
}

var inscope = &goroutineLocal{
	store: make(map[int64]*BulkChange),
}

func (gl *goroutineLocal) Get(gid int64) *BulkChange {
	gl.mu.RLock()
	defer gl.mu.RUnlock()
	return gl.store[gid]
}

func (gl *goroutineLocal) Set(gid int64, bc *BulkChange) {
	gl.mu.Lock()
	defer gl.mu.Unlock()
	if bc == nil {
		delete(gl.store, gid)
	} else {
		gl.store[gid] = bc
	}
}

// getGoroutineID: 현재 goroutine의 고유 ID를 반환
// 데모 용도로 간단하게 구현 — 메인 goroutine은 0, 나머지는 카운터 기반
var (
	goroutineIDCounter int64
	goroutineIDMu      sync.Mutex
	goroutineIDMap     = make(map[string]int64)
)

func getGoroutineID() int64 {
	// 데모에서는 단일 goroutine이므로 0을 반환
	return 0
}

type BulkChange struct {
	saveable  Saveable
	parent    *BulkChange // 중첩 BulkChange를 위한 부모 참조
	completed bool
	gid       int64 // 이 BulkChange가 생성된 goroutine ID
}

// NewBulkChange: BulkChange를 생성하고 현재 스코프에 설정한다.
// 실제 Jenkins 구현:
//   public BulkChange(Saveable saveable) {
//       this.parent = current();       // 이전 BulkChange를 parent로 저장
//       this.saveable = saveable;
//       allocator = new Exception();   // 디버깅용 스택트레이스 저장
//       INSCOPE.set(this);             // 자신을 현재 스코프로 설정
//   }
func NewBulkChange(saveable Saveable) *BulkChange {
	gid := getGoroutineID()
	bc := &BulkChange{
		saveable: saveable,
		parent:   CurrentBulkChange(), // 이전 BulkChange를 parent로 저장 (중첩 지원)
		gid:      gid,
	}
	inscope.Set(gid, bc) // 자신을 현재 스코프로 설정
	return bc
}

// CurrentBulkChange: 현재 goroutine에서 활성화된 BulkChange를 반환한다.
// 실제: public static BulkChange current() { return INSCOPE.get(); }
func CurrentBulkChange() *BulkChange {
	return inscope.Get(getGoroutineID())
}

// Contains: 주어진 Saveable이 현재 활성 BulkChange 체인에 포함되는지 확인한다.
// BulkChange 체인을 parent → parent로 순회하면서 검사한다.
//
// 실제 Jenkins 구현:
//   public static boolean contains(Saveable s) {
//       for (BulkChange b = current(); b != null; b = b.parent)
//           if (b.saveable == s || b.saveable == ALL)
//               return true;
//       return false;
//   }
//
// ALL 매직 인스턴스: 모든 Saveable에 대해 true를 반환하는 특수 Saveable.
// Jenkins.doConfigSubmit()에서 전역 설정 변경 시 사용된다.
func BulkChangeContains(s Saveable) bool {
	for b := CurrentBulkChange(); b != nil; b = b.parent {
		if b.saveable == s || b.saveable == bulkChangeALL {
			return true
		}
	}
	return false
}

// bulkChangeALL: 모든 save()를 억제하는 매직 Saveable
// 실제: public static final Saveable ALL = () -> {};
var bulkChangeALL Saveable = &noopSaveable{name: "BulkChange.ALL"}

type noopSaveable struct{ name string }

func (n *noopSaveable) Save() error   { return nil }
func (n *noopSaveable) Name() string  { return n.name }

// Commit: 변경사항을 저장한다.
// 핵심: 자신을 스코프에서 먼저 제거한 후 save()를 호출한다.
// 이렇게 해야 save() 내부에서 BulkChange.contains()가 false를 반환하여 실제 저장이 실행된다.
//
// 실제 Jenkins 구현:
//   public void commit() throws IOException {
//       if (completed) return;
//       completed = true;
//       pop();              // 스코프에서 제거 (먼저!)
//       saveable.save();    // 그 다음 실제 저장
//   }
func (bc *BulkChange) Commit() error {
	if bc.completed {
		return nil
	}
	bc.completed = true
	bc.pop() // 스코프에서 먼저 제거
	return bc.saveable.Save() // 그 다음 실제 저장
}

// Abort: 저장하지 않고 스코프를 해제한다.
// commit() 후에 호출해도 안전하다 (try-with-resources 패턴).
//
// 실제 Jenkins 구현:
//   public void abort() {
//       if (completed) return;
//       completed = true;
//       pop();
//   }
func (bc *BulkChange) Abort() {
	if bc.completed {
		return
	}
	bc.completed = true
	bc.pop()
}

// Close: Abort()의 별칭. Go의 defer 패턴을 위해 제공.
// 실제: public void close() { abort(); }
func (bc *BulkChange) Close() {
	bc.Abort()
}

// pop: 스코프 스택에서 자신을 제거하고 parent를 복원한다.
// 실제 Jenkins 구현:
//   private void pop() {
//       if (current() != this)
//           throw new AssertionError("Trying to save BulkChange that's not in scope");
//       INSCOPE.set(parent);
//   }
func (bc *BulkChange) pop() {
	current := CurrentBulkChange()
	if current != bc {
		panic("스코프에 없는 BulkChange를 pop하려고 시도")
	}
	inscope.Set(bc.gid, bc.parent) // parent를 현재 스코프로 복원
}

// ============================================================================
// 5. JenkinsConfig — BulkChange와 협력하는 Saveable 구현체
// ============================================================================
// Jenkins.java의 save() 메서드를 모델링한다.
// 실제 코드 (jenkins/core/src/main/java/jenkins/model/Jenkins.java, 3579행):
//
//   public void save() throws IOException {
//       if (BulkChange.contains(this)) {  ← BulkChange 활성 시 저장 억제
//           return;
//       }
//       getConfigFile().write(this);
//       SaveableListener.fireOnChange(this, getConfigFile());
//   }

type JenkinsConfig struct {
	XMLName       xml.Name `xml:"config"`
	SystemMessage string   `xml:"systemMessage"`
	NumExecutors  int      `xml:"numExecutors"`
	Mode          string   `xml:"mode"`
	QuietPeriod   int      `xml:"quietPeriod"`
	SCMCheckout   int      `xml:"scmCheckoutRetryCount"`

	filePath  string
	saveCount int // save() 호출 횟수 추적 (데모용)
	diskWrites int // 실제 디스크 쓰기 횟수 추적 (데모용)
}

func NewJenkinsConfig(filePath string) *JenkinsConfig {
	return &JenkinsConfig{
		SystemMessage: "Welcome to Jenkins",
		NumExecutors:  2,
		Mode:          "NORMAL",
		QuietPeriod:   5,
		SCMCheckout:   0,
		filePath:      filePath,
	}
}

func (c *JenkinsConfig) Name() string { return "JenkinsConfig" }

// Save: BulkChange.contains() 확인 후 실제 저장 수행
// Jenkins.java의 save() 패턴을 정확히 따른다.
func (c *JenkinsConfig) Save() error {
	c.saveCount++

	// 핵심: BulkChange가 활성이면 실제 저장을 억제한다
	// 실제: if (BulkChange.contains(this)) { return; }
	if BulkChangeContains(c) {
		fmt.Printf("  [Save #%d] BulkChange 활성 → 저장 억제됨 (%s)\n", c.saveCount, c.Name())
		return nil
	}

	fmt.Printf("  [Save #%d] 디스크에 저장 실행 → %s\n", c.saveCount, c.filePath)
	c.diskWrites++

	// AtomicFileWriter를 사용한 원자적 쓰기
	writer, err := NewAtomicFileWriter(c.filePath)
	if err != nil {
		return err
	}

	data, err := xml.MarshalIndent(c, "", "  ")
	if err != nil {
		writer.Abort()
		return fmt.Errorf("XML 직렬화 실패: %w", err)
	}

	xmlHeader := []byte("<?xml version='1.1' encoding='UTF-8'?>\n")
	if _, err := writer.Write(append(xmlHeader, data...)); err != nil {
		writer.Abort()
		return fmt.Errorf("임시 파일 쓰기 실패: %w", err)
	}

	if err := writer.Commit(); err != nil {
		return err
	}

	// 리스너 통지
	// 실제: SaveableListener.fireOnChange(this, getConfigFile());
	globalListenerRegistry.FireOnChange(c, c.filePath)
	return nil
}

// 설정 변경 메서드들 — 각 메서드가 내부적으로 Save()를 호출한다.
// BulkChange가 없으면 매번 디스크에 저장, 있으면 억제된다.

func (c *JenkinsConfig) SetSystemMessage(msg string) error {
	c.SystemMessage = msg
	return c.Save()
}

func (c *JenkinsConfig) SetNumExecutors(n int) error {
	c.NumExecutors = n
	return c.Save()
}

func (c *JenkinsConfig) SetMode(mode string) error {
	c.Mode = mode
	return c.Save()
}

func (c *JenkinsConfig) SetQuietPeriod(period int) error {
	c.QuietPeriod = period
	return c.Save()
}

func (c *JenkinsConfig) SetSCMCheckoutRetryCount(count int) error {
	c.SCMCheckout = count
	return c.Save()
}

// ============================================================================
// 6. JobConfig — 잡 설정 Saveable 구현체
// ============================================================================
// Job.java의 save() 메서드를 모델링한다.
// 실제 코드에서도 BulkChange.contains(this) 패턴을 동일하게 사용한다.

type JobConfig struct {
	XMLName     xml.Name `xml:"project"`
	DisplayName string   `xml:"displayName"`
	Description string   `xml:"description"`
	Disabled    bool     `xml:"disabled"`
	Concurrent  bool     `xml:"concurrentBuild"`

	filePath   string
	saveCount  int
	diskWrites int
}

func NewJobConfig(name, filePath string) *JobConfig {
	return &JobConfig{
		DisplayName: name,
		Description: "",
		filePath:    filePath,
	}
}

func (j *JobConfig) Name() string { return fmt.Sprintf("Job[%s]", j.DisplayName) }

func (j *JobConfig) Save() error {
	j.saveCount++

	if BulkChangeContains(j) {
		fmt.Printf("  [Save #%d] BulkChange 활성 → 저장 억제됨 (%s)\n", j.saveCount, j.Name())
		return nil
	}

	fmt.Printf("  [Save #%d] 디스크에 저장 실행 → %s\n", j.saveCount, j.filePath)
	j.diskWrites++

	writer, err := NewAtomicFileWriter(j.filePath)
	if err != nil {
		return err
	}

	data, err := xml.MarshalIndent(j, "", "  ")
	if err != nil {
		writer.Abort()
		return err
	}

	xmlHeader := []byte("<?xml version='1.1' encoding='UTF-8'?>\n")
	if _, err := writer.Write(append(xmlHeader, data...)); err != nil {
		writer.Abort()
		return err
	}

	if err := writer.Commit(); err != nil {
		return err
	}

	globalListenerRegistry.FireOnChange(j, j.filePath)
	return nil
}

func (j *JobConfig) SetDescription(desc string) error {
	j.Description = desc
	return j.Save()
}

func (j *JobConfig) SetDisabled(disabled bool) error {
	j.Disabled = disabled
	return j.Save()
}

func (j *JobConfig) SetConcurrentBuild(concurrent bool) error {
	j.Concurrent = concurrent
	return j.Save()
}

// ============================================================================
// 메인 — 데모 실행
// ============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Jenkins BulkChange 저장 트랜잭션 패턴 시뮬레이션               ║")
	fmt.Println("║  참조: jenkins/core/src/main/java/hudson/BulkChange.java        ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════╝")

	// 임시 디렉토리 생성
	tmpDir, err := os.MkdirTemp("", "jenkins-bulk-change-*")
	if err != nil {
		fmt.Printf("임시 디렉토리 생성 실패: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)

	// SaveableListener 등록
	listener := &LoggingSaveableListener{}
	globalListenerRegistry.Register(listener)

	// ================================================================
	// 데모 1: BulkChange 없이 저장 — 매 변경마다 디스크 I/O
	// ================================================================
	fmt.Println("\n" + strings.Repeat("=", 66))
	fmt.Println("데모 1: BulkChange 없이 저장 — 매 변경마다 디스크 I/O 발생")
	fmt.Println(strings.Repeat("=", 66))
	fmt.Println()
	fmt.Println("BulkChange 없이 5개의 설정을 변경하면 5번의 디스크 쓰기가 발생한다.")
	fmt.Println("이것은 Jenkins 웹 UI에서 설정 페이지의 개별 필드를 수정할 때")
	fmt.Println("BulkChange를 사용하지 않는 경우에 해당한다.\n")

	configPath := filepath.Join(tmpDir, "config.xml")
	config := NewJenkinsConfig(configPath)

	start := time.Now()

	// 각 변경마다 save() → 디스크 I/O 발생
	config.SetSystemMessage("CI/CD Server")
	config.SetNumExecutors(4)
	config.SetMode("EXCLUSIVE")
	config.SetQuietPeriod(10)
	config.SetSCMCheckoutRetryCount(3)

	elapsed := time.Since(start)

	fmt.Printf("\n  결과: save() %d회 호출, 디스크 쓰기 %d회, 소요시간 %v\n",
		config.saveCount, config.diskWrites, elapsed)

	// 저장된 파일 내용 확인
	printFileContent(configPath, "  ")

	// ================================================================
	// 데모 2: BulkChange로 배치 저장 — 한 번만 디스크 I/O
	// ================================================================
	fmt.Println("\n" + strings.Repeat("=", 66))
	fmt.Println("데모 2: BulkChange로 배치 저장 — 한 번만 디스크 I/O")
	fmt.Println(strings.Repeat("=", 66))
	fmt.Println()
	fmt.Println("BulkChange(config)를 열면 config.save()가 억제된다.")
	fmt.Println("commit() 호출 시에만 실제 저장이 한 번 실행된다.")
	fmt.Println()
	fmt.Println("실제 패턴 (Jenkins.java doConfigSubmit, 4034행):")
	fmt.Println("  try (BulkChange bc = new BulkChange(this)) {")
	fmt.Println("      // ... 여러 설정 변경 ...")
	fmt.Println("      bc.commit();")
	fmt.Println("  }\n")

	configPath2 := filepath.Join(tmpDir, "config2.xml")
	config2 := NewJenkinsConfig(configPath2)

	start = time.Now()

	// BulkChange 시작 — try-with-resources 패턴을 Go defer로 재현
	bc := NewBulkChange(config2)
	defer bc.Close() // close()는 abort()의 별칭 — commit 후에는 no-op

	// 5개 변경 — save() 호출되지만 BulkChange.contains()가 true이므로 억제
	config2.SetSystemMessage("Production CI Server")
	config2.SetNumExecutors(8)
	config2.SetMode("EXCLUSIVE")
	config2.SetQuietPeriod(0)
	config2.SetSCMCheckoutRetryCount(5)

	// commit() — 스코프에서 먼저 제거 후 save() 한 번 실행
	fmt.Println("\n  → bc.commit() 호출:")
	if err := bc.Commit(); err != nil {
		fmt.Printf("  commit 실패: %v\n", err)
	}

	elapsed = time.Since(start)

	fmt.Printf("\n  결과: save() %d회 호출, 디스크 쓰기 %d회 (5회→1회로 감소), 소요시간 %v\n",
		config2.saveCount, config2.diskWrites, elapsed)

	printFileContent(configPath2, "  ")

	// ================================================================
	// 데모 3: AtomicFileWriter 원자적 쓰기 시뮬레이션
	// ================================================================
	fmt.Println("\n" + strings.Repeat("=", 66))
	fmt.Println("데모 3: AtomicFileWriter — 원자적 파일 쓰기")
	fmt.Println(strings.Repeat("=", 66))
	fmt.Println()
	fmt.Println("AtomicFileWriter의 핵심: 임시 파일에 쓰기 → commit() 시 rename.")
	fmt.Println("크래시가 발생해도 원본 파일이 손상되지 않는다.\n")

	demoAtomicFileWriter(tmpDir)

	// ================================================================
	// 데모 4: 중첩 BulkChange
	// ================================================================
	fmt.Println("\n" + strings.Repeat("=", 66))
	fmt.Println("데모 4: 중첩 BulkChange — parent 체인을 통한 스택 관리")
	fmt.Println(strings.Repeat("=", 66))
	fmt.Println()
	fmt.Println("BulkChange는 중첩될 수 있다. 내부 BulkChange는 parent로 외부를 참조한다.")
	fmt.Println("contains()는 체인을 순회하며 모든 상위 BulkChange를 확인한다.\n")
	fmt.Println("  실제 구현 (BulkChange.java, 151행):")
	fmt.Println("    public static boolean contains(Saveable s) {")
	fmt.Println("        for (BulkChange b = current(); b != null; b = b.parent)")
	fmt.Println("            if (b.saveable == s || b.saveable == ALL)")
	fmt.Println("                return true;")
	fmt.Println("        return false;")
	fmt.Println("    }\n")

	demoNestedBulkChange(tmpDir)

	// ================================================================
	// 데모 5: BulkChange.ALL — 모든 저장 억제
	// ================================================================
	fmt.Println("\n" + strings.Repeat("=", 66))
	fmt.Println("데모 5: BulkChange.ALL — 모든 Saveable의 저장 억제")
	fmt.Println(strings.Repeat("=", 66))
	fmt.Println()
	fmt.Println("BulkChange.ALL은 매직 Saveable 인스턴스로,")
	fmt.Println("어떤 Saveable에 대해서도 contains()가 true를 반환하게 만든다.")
	fmt.Println("전역 설정 변경 시 모든 저장을 한 번에 억제할 때 사용한다.\n")
	fmt.Println("  실제 구현 (BulkChange.java, 163행):")
	fmt.Println("    public static final Saveable ALL = () -> {};\n")

	demoBulkChangeAll(tmpDir)

	// ================================================================
	// 데모 6: BulkChange abort — 저장 없이 롤백
	// ================================================================
	fmt.Println("\n" + strings.Repeat("=", 66))
	fmt.Println("데모 6: BulkChange abort — commit 없이 close하면 저장 안 됨")
	fmt.Println(strings.Repeat("=", 66))
	fmt.Println()
	fmt.Println("commit()을 호출하지 않고 close()하면 abort()가 실행되어")
	fmt.Println("변경사항이 저장되지 않는다. 에러 발생 시의 안전장치이다.\n")
	fmt.Println("  실제 패턴:")
	fmt.Println("    try (BulkChange bc = new BulkChange(saveable)) {")
	fmt.Println("        // ... 변경 중 에러 발생 ...")
	fmt.Println("        // bc.commit()을 호출하지 않음")
	fmt.Println("    } // close() → abort() → 저장하지 않음\n")

	demoBulkChangeAbort(tmpDir)

	// ================================================================
	// 데모 7: SaveableListener 이벤트
	// ================================================================
	fmt.Println("\n" + strings.Repeat("=", 66))
	fmt.Println("데모 7: SaveableListener — 저장 이벤트 통합 확인")
	fmt.Println(strings.Repeat("=", 66))
	fmt.Println()
	fmt.Println("전체 데모에서 발생한 SaveableListener 이벤트 목록:\n")

	for i, event := range listener.events {
		fmt.Printf("  %2d. %s\n", i+1, strings.TrimPrefix(event, "  "))
	}
	fmt.Printf("\n  총 %d건의 저장 이벤트 발생\n", len(listener.events))

	fmt.Println("\n" + strings.Repeat("=", 66))
	fmt.Println("시뮬레이션 완료")
	fmt.Println(strings.Repeat("=", 66))
}

// ============================================================================
// 데모 함수들
// ============================================================================

// demoAtomicFileWriter: AtomicFileWriter의 commit/abort 동작을 시연한다.
func demoAtomicFileWriter(tmpDir string) {
	// 성공 케이스: commit
	fmt.Println("  --- 성공 케이스: commit ---")
	destPath := filepath.Join(tmpDir, "atomic-test.xml")

	// 원본 파일 먼저 생성
	os.WriteFile(destPath, []byte("<original>data</original>"), 0644)
	fmt.Printf("  원본 파일: %s\n", destPath)
	printFileContent(destPath, "    ")

	writer, err := NewAtomicFileWriter(destPath)
	if err != nil {
		fmt.Printf("  Writer 생성 실패: %v\n", err)
		return
	}
	fmt.Printf("  임시 파일 생성: %s\n", writer.tmpPath)

	newContent := "<updated>new data</updated>"
	writer.Write([]byte(newContent))
	fmt.Printf("  임시 파일에 쓰기 완료\n")

	// 이 시점에서 원본은 아직 변경되지 않음
	origData, _ := os.ReadFile(destPath)
	fmt.Printf("  commit 전 원본 파일 내용: %s\n", string(origData))

	// commit: rename으로 원자적 교체
	if err := writer.Commit(); err != nil {
		fmt.Printf("  commit 실패: %v\n", err)
		return
	}

	updatedData, _ := os.ReadFile(destPath)
	fmt.Printf("  commit 후 파일 내용: %s\n", string(updatedData))

	// 실패 케이스: abort
	fmt.Println("\n  --- 실패 케이스: abort ---")
	writer2, _ := NewAtomicFileWriter(destPath)
	fmt.Printf("  임시 파일 생성: %s\n", writer2.tmpPath)
	writer2.Write([]byte("<corrupted>bad data</corrupted>"))
	fmt.Println("  임시 파일에 쓰기 완료 (잘못된 데이터)")

	// abort: 임시 파일 삭제, 원본 보존
	writer2.Abort()
	fmt.Println("  abort() 호출 → 임시 파일 삭제")

	preservedData, _ := os.ReadFile(destPath)
	fmt.Printf("  abort 후 원본 파일 내용 (보존됨): %s\n", string(preservedData))

	// 임시 파일이 삭제되었는지 확인
	if _, err := os.Stat(writer2.tmpPath); os.IsNotExist(err) {
		fmt.Println("  임시 파일이 정상적으로 삭제됨 ✓")
	}
}

// demoNestedBulkChange: 중첩 BulkChange 동작을 시연한다.
func demoNestedBulkChange(tmpDir string) {
	configPath := filepath.Join(tmpDir, "nested-config.xml")
	jobPath := filepath.Join(tmpDir, "nested-job.xml")

	config := NewJenkinsConfig(configPath)
	job := NewJobConfig("my-pipeline", jobPath)

	fmt.Println("  --- 중첩 BulkChange 시작 ---")
	fmt.Println()

	// 외부 BulkChange: config 대상
	fmt.Println("  [1] 외부 BulkChange 생성 (config)")
	outerBC := NewBulkChange(config)

	config.SetSystemMessage("Nested Test")
	config.SetNumExecutors(16)
	fmt.Println()

	// 내부 BulkChange: job 대상
	fmt.Println("  [2] 내부 BulkChange 생성 (job)")
	innerBC := NewBulkChange(job)

	// BulkChange 스택 상태 출력
	fmt.Println("\n  현재 BulkChange 스택:")
	fmt.Println("    current → innerBC(job)")
	fmt.Println("    parent  → outerBC(config)")
	fmt.Println("    parent  → nil")
	fmt.Println()

	// config는 외부 BulkChange에 의해 여전히 억제됨
	fmt.Printf("  BulkChangeContains(config) = %v (외부 BC에 의해 억제)\n",
		BulkChangeContains(config))
	fmt.Printf("  BulkChangeContains(job)    = %v (내부 BC에 의해 억제)\n",
		BulkChangeContains(job))
	fmt.Println()

	job.SetDescription("Pipeline job")
	job.SetDisabled(false)
	fmt.Println()

	// 내부 BulkChange commit — job만 저장
	fmt.Println("  [3] innerBC.commit() → job 저장")
	innerBC.Commit()
	fmt.Println()

	// 내부 commit 후 상태
	fmt.Printf("  내부 commit 후 BulkChangeContains(config) = %v (외부 BC 여전히 활성)\n",
		BulkChangeContains(config))
	fmt.Printf("  내부 commit 후 BulkChangeContains(job)    = %v (내부 BC 종료됨)\n",
		BulkChangeContains(job))
	fmt.Println()

	// 외부 BulkChange commit — config 저장
	fmt.Println("  [4] outerBC.commit() → config 저장")
	outerBC.Commit()
	fmt.Println()

	fmt.Printf("  config: save() %d회 호출, 디스크 쓰기 %d회\n",
		config.saveCount, config.diskWrites)
	fmt.Printf("  job:    save() %d회 호출, 디스크 쓰기 %d회\n",
		job.saveCount, job.diskWrites)
}

// demoBulkChangeAll: BulkChange.ALL의 동작을 시연한다.
func demoBulkChangeAll(tmpDir string) {
	configPath := filepath.Join(tmpDir, "all-config.xml")
	jobPath := filepath.Join(tmpDir, "all-job.xml")

	config := NewJenkinsConfig(configPath)
	job := NewJobConfig("all-test", jobPath)

	// BulkChange.ALL로 생성하면 모든 Saveable의 저장이 억제된다
	fmt.Println("  BulkChange(ALL) 생성 → 모든 Saveable 저장 억제")
	bcAll := NewBulkChange(bulkChangeALL)

	fmt.Printf("  BulkChangeContains(config) = %v\n", BulkChangeContains(config))
	fmt.Printf("  BulkChangeContains(job)    = %v\n", BulkChangeContains(job))
	fmt.Println()

	config.SetSystemMessage("ALL test")
	job.SetDescription("ALL test job")
	fmt.Println()

	fmt.Printf("  config: save() %d회, 디스크 쓰기 %d회 (억제됨)\n",
		config.saveCount, config.diskWrites)
	fmt.Printf("  job:    save() %d회, 디스크 쓰기 %d회 (억제됨)\n",
		job.saveCount, job.diskWrites)

	// abort — ALL이므로 어떤 Saveable도 저장하지 않고 종료
	bcAll.Abort()
	fmt.Println("\n  bcAll.abort() → 저장 없이 종료")
}

// demoBulkChangeAbort: commit 없이 close하면 저장되지 않음을 시연한다.
func demoBulkChangeAbort(tmpDir string) {
	configPath := filepath.Join(tmpDir, "abort-config.xml")

	// 원본 상태 저장
	config := NewJenkinsConfig(configPath)
	config.Save() // 초기 상태 저장
	fmt.Printf("  초기 저장 완료 (systemMessage: %q)\n", config.SystemMessage)
	initialDiskWrites := config.diskWrites

	// BulkChange 시작
	fmt.Println("\n  BulkChange 시작 — 변경 후 commit 없이 close")
	bc := NewBulkChange(config)

	config.SetSystemMessage("이 메시지는 저장되지 않을 것이다")
	config.SetNumExecutors(100)
	config.SetMode("BROKEN")
	fmt.Println()

	fmt.Printf("  메모리 상태 — systemMessage: %q (변경됨)\n", config.SystemMessage)
	fmt.Printf("  save() 호출 %d회, 디스크 쓰기 %d회 (모두 억제)\n",
		config.saveCount, config.diskWrites)

	// commit()을 호출하지 않고 close()
	bc.Close() // → abort() → 저장 안 함
	fmt.Println("\n  bc.close() 호출 (commit 안 함) → abort 실행")

	fmt.Printf("  디스크 쓰기는 초기 저장(%d회) 이후 증가하지 않음: %d회\n",
		initialDiskWrites, config.diskWrites)

	// 디스크의 파일은 초기 상태 유지
	printFileContent(configPath, "  ")

	fmt.Println("\n  주의: BulkChange는 메모리 상태를 롤백하지 않는다!")
	fmt.Println("  실제 Jenkins 주석 (BulkChange.java, 113행):")
	fmt.Println("    \"unlike a real transaction, this will not roll back the state of the object\"")
	fmt.Printf("  메모리 — systemMessage: %q (여전히 변경된 상태)\n", config.SystemMessage)
}

// printFileContent: 파일 내용을 출력한다.
func printFileContent(path, indent string) {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Printf("%s파일 읽기 실패: %v\n", indent, err)
		return
	}
	fmt.Printf("%s저장된 파일 내용 (%s):\n", indent, filepath.Base(path))
	for _, line := range strings.Split(string(data), "\n") {
		fmt.Printf("%s  %s\n", indent, line)
	}
}
