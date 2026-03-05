// poc-09-xml-persistence: Jenkins XStream/XmlFile XML 영속성 시뮬레이션
//
// Jenkins는 모든 설정과 빌드 데이터를 XML 파일로 저장한다 (JENKINS_HOME 아래).
// 이 PoC는 Jenkins의 XML 영속성 시스템 핵심 개념을 Go 표준 라이브러리만으로 재현한다.
//
// 참조 소스 코드:
//   - jenkins/core/src/main/java/hudson/XmlFile.java
//     : read() → XML에서 객체 로드, write(Object) → 객체를 XML로 저장
//   - jenkins/core/src/main/java/hudson/util/AtomicFileWriter.java
//     : 임시파일에 쓰고 rename하는 원자적 쓰기 구현
//   - jenkins/core/src/main/java/hudson/BulkChange.java
//     : ThreadLocal 스택 기반, 여러 save() 호출을 commit()까지 지연
//   - jenkins/core/src/main/java/hudson/model/Saveable.java
//     : save() 인터페이스 — XML 영속 대상 객체의 공통 계약
//   - jenkins/core/src/main/java/hudson/model/listeners/SaveableListener.java
//     : onChange(Saveable, XmlFile) — 저장 이벤트 리스너
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
// save()가 호출되면 BulkChange 스코프 확인 → 스코프 밖이면 즉시 저장.
//
//   public interface Saveable {
//       void save() throws IOException;
//       Saveable NOOP = () -> {};
//   }

type Saveable interface {
	Save() error
	GetXmlFile() *XmlFile
}

// ============================================================================
// 2. SaveableListener — jenkins/core/.../listeners/SaveableListener.java
// ============================================================================
// Jenkins는 Saveable 객체가 저장될 때 리스너에게 알린다.
// 실제 코드에서는 ExtensionPoint로 등록되며, fireOnChange 정적 메서드로 호출.
//
//   public static void fireOnChange(Saveable o, XmlFile file) {
//       Listeners.notify(SaveableListener.class, false, l -> l.onChange(o, file));
//   }

type SaveableListener interface {
	OnChange(s Saveable, file *XmlFile)
	OnDeleted(s Saveable, file *XmlFile)
}

// SaveableListenerRegistry: 전역 리스너 관리
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

func (r *SaveableListenerRegistry) FireOnChange(s Saveable, file *XmlFile) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, l := range r.listeners {
		l.OnChange(s, file)
	}
}

func (r *SaveableListenerRegistry) FireOnDeleted(s Saveable, file *XmlFile) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, l := range r.listeners {
		l.OnDeleted(s, file)
	}
}

// LoggingSaveableListener: 저장 이벤트를 로깅하는 리스너 구현
type LoggingSaveableListener struct {
	events []string
}

func (l *LoggingSaveableListener) OnChange(s Saveable, file *XmlFile) {
	msg := fmt.Sprintf("[SaveableListener] onChange: %s", file.FilePath)
	l.events = append(l.events, msg)
	fmt.Println(msg)
}

func (l *LoggingSaveableListener) OnDeleted(s Saveable, file *XmlFile) {
	msg := fmt.Sprintf("[SaveableListener] onDeleted: %s", file.FilePath)
	l.events = append(l.events, msg)
	fmt.Println(msg)
}

// ============================================================================
// 3. AtomicFileWriter — jenkins/core/.../util/AtomicFileWriter.java
// ============================================================================
// Jenkins의 AtomicFileWriter는 임시 파일에 먼저 쓰고, 완료 후 rename(원자적 이동)하여
// 크래시 시에도 원본 파일이 깨지지 않도록 보장한다.
//
// 핵심 흐름 (AtomicFileWriter.java 라인 138~238):
//   1. File.createTempFile() → 임시 파일 생성
//   2. write() → 임시 파일에 데이터 기록
//   3. commit() → close() → Files.move(source, dest, ATOMIC_MOVE) → 원자적 rename
//   4. abort() → close() → Files.deleteIfExists(tmpPath) → 임시 파일 삭제
//
// Go에서는 os.Rename()이 같은 파일시스템 내에서 원자적이므로 동일한 패턴을 사용한다.

type AtomicFileWriter struct {
	destPath string   // 최종 목적지 경로
	tmpPath  string   // 임시 파일 경로
	tmpFile  *os.File // 임시 파일 핸들
	closed   bool     // 닫힘 여부
}

// NewAtomicFileWriter: 원자적 파일 쓰기를 위한 Writer 생성
// Jenkins 코드에서:
//   tmpPath = File.createTempFile(destPath.getFileName() + "-atomic", "tmp", dir.toFile()).toPath()
func NewAtomicFileWriter(destPath string) (*AtomicFileWriter, error) {
	dir := filepath.Dir(destPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("디렉토리 생성 실패: %w", err)
	}

	base := filepath.Base(destPath)
	tmpFile, err := os.CreateTemp(dir, base+"-atomic-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("임시 파일 생성 실패: %w", err)
	}

	return &AtomicFileWriter{
		destPath: destPath,
		tmpPath:  tmpFile.Name(),
		tmpFile:  tmpFile,
		closed:   false,
	}, nil
}

// Write: 임시 파일에 데이터 기록
func (w *AtomicFileWriter) Write(data []byte) (int, error) {
	if w.closed {
		return 0, fmt.Errorf("이미 닫힌 writer")
	}
	return w.tmpFile.Write(data)
}

// WriteString: 문자열 기록
func (w *AtomicFileWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

// Commit: 임시 파일을 최종 목적지로 원자적 이동
// Jenkins 코드에서 (AtomicFileWriter.java 라인 213~238):
//   public void commit() throws IOException {
//       close();
//       try { move(tmpPath, destPath); }
//       finally { Files.deleteIfExists(tmpPath); }
//   }
//   private static void move(Path source, Path destination) {
//       Files.move(source, destination, StandardCopyOption.ATOMIC_MOVE);
//   }
func (w *AtomicFileWriter) Commit() error {
	if !w.closed {
		if err := w.tmpFile.Sync(); err != nil {
			return fmt.Errorf("fsync 실패: %w", err)
		}
		if err := w.tmpFile.Close(); err != nil {
			return fmt.Errorf("임시 파일 닫기 실패: %w", err)
		}
		w.closed = true
	}
	// 원자적 이동 (같은 파일시스템 내에서 os.Rename은 원자적)
	if err := os.Rename(w.tmpPath, w.destPath); err != nil {
		// 원자적 이동 실패 시 임시 파일 정리
		os.Remove(w.tmpPath)
		return fmt.Errorf("원자적 이동 실패: %w", err)
	}
	return nil
}

// Abort: 임시 파일을 삭제하고 원본을 유지
// Jenkins 코드에서 (AtomicFileWriter.java 라인 204~211):
//   public void abort() throws IOException {
//       try { close(); }
//       finally { Files.deleteIfExists(tmpPath); }
//   }
func (w *AtomicFileWriter) Abort() error {
	if !w.closed {
		w.tmpFile.Close()
		w.closed = true
	}
	return os.Remove(w.tmpPath)
}

// ============================================================================
// 4. XmlFile — jenkins/core/src/main/java/hudson/XmlFile.java
// ============================================================================
// Jenkins의 XmlFile은 XML 데이터 파일을 관리하는 핵심 클래스이다.
// XStream 라이브러리를 사용하여 Java 객체 ↔ XML 직렬화/역직렬화를 수행한다.
//
// 핵심 메서드 (XmlFile.java):
//   - read(): XML 파일 → 객체 (xs.fromXML(in))
//   - write(Object): 객체 → XML 파일 (AtomicFileWriter 사용)
//   - unmarshal(Object): 기존 객체에 XML 데이터 로드
//   - exists(), delete(), mkdirs()
//
// write() 메서드 핵심 (XmlFile.java 라인 203~227):
//   public void write(Object o) throws IOException {
//       mkdirs();
//       AtomicFileWriter w = new AtomicFileWriter(file);
//       try {
//           w.write("<?xml version='1.1' encoding='UTF-8'?>\n");
//           beingWritten.put(o, null);
//           writing.set(file);
//           try { xs.toXML(o, w); }
//           finally { beingWritten.remove(o); writing.set(null); }
//           w.commit();
//       } finally { w.abort(); }
//   }

type XmlFile struct {
	FilePath string
}

// NewXmlFile: XmlFile 생성
func NewXmlFile(path string) *XmlFile {
	return &XmlFile{FilePath: path}
}

// Read: XML 파일에서 객체 로드 (encoding/xml 사용)
// Jenkins에서는 XStream의 fromXML()을 사용한다.
// Go에서는 encoding/xml의 Unmarshal을 사용한다.
func (xf *XmlFile) Read(target interface{}) error {
	data, err := os.ReadFile(xf.FilePath)
	if err != nil {
		return fmt.Errorf("[XmlFile] 파일 읽기 실패 %s: %w", xf.FilePath, err)
	}
	if err := xml.Unmarshal(data, target); err != nil {
		return fmt.Errorf("[XmlFile] XML 역직렬화 실패 %s: %w", xf.FilePath, err)
	}
	fmt.Printf("[XmlFile] 읽기 완료: %s\n", xf.FilePath)
	return nil
}

// Write: 객체를 XML 파일로 저장 (AtomicFileWriter 사용)
// Jenkins의 write() 메서드와 동일한 패턴:
//   1. AtomicFileWriter 생성
//   2. XML 선언 + 직렬화된 데이터 기록
//   3. commit() 호출 → 원자적 교체
//   4. 실패 시 abort() 호출 → 임시 파일 정리
func (xf *XmlFile) Write(obj interface{}) error {
	// 디렉토리 생성
	dir := filepath.Dir(xf.FilePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("[XmlFile] 디렉토리 생성 실패: %w", err)
	}

	// AtomicFileWriter 생성 — Jenkins의 패턴 그대로
	writer, err := NewAtomicFileWriter(xf.FilePath)
	if err != nil {
		return fmt.Errorf("[XmlFile] AtomicFileWriter 생성 실패: %w", err)
	}

	// Jenkins에서는 try-finally로 abort()를 보장
	success := false
	defer func() {
		if !success {
			writer.Abort()
		}
	}()

	// XML 선언 작성 — Jenkins에서는 "<?xml version='1.1' encoding='UTF-8'?>\n"
	if _, err := writer.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"); err != nil {
		return fmt.Errorf("[XmlFile] XML 선언 쓰기 실패: %w", err)
	}

	// 객체를 XML로 직렬화
	data, err := xml.MarshalIndent(obj, "", "  ")
	if err != nil {
		return fmt.Errorf("[XmlFile] XML 직렬화 실패: %w", err)
	}
	if _, err := writer.Write(data); err != nil {
		return fmt.Errorf("[XmlFile] XML 데이터 쓰기 실패: %w", err)
	}
	if _, err := writer.WriteString("\n"); err != nil {
		return fmt.Errorf("[XmlFile] 개행 쓰기 실패: %w", err)
	}

	// 원자적 커밋
	if err := writer.Commit(); err != nil {
		return fmt.Errorf("[XmlFile] 커밋 실패: %w", err)
	}
	success = true
	fmt.Printf("[XmlFile] 쓰기 완료: %s\n", xf.FilePath)
	return nil
}

// Exists: 파일 존재 여부 확인
func (xf *XmlFile) Exists() bool {
	_, err := os.Stat(xf.FilePath)
	return err == nil
}

// Delete: 파일 삭제
func (xf *XmlFile) Delete() error {
	return os.Remove(xf.FilePath)
}

// AsString: 파일 내용을 문자열로 반환
func (xf *XmlFile) AsString() (string, error) {
	data, err := os.ReadFile(xf.FilePath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ============================================================================
// 5. BulkChange — jenkins/core/src/main/java/hudson/BulkChange.java
// ============================================================================
// BulkChange는 여러 변경사항을 하나의 트랜잭션처럼 묶는 패턴이다.
// ThreadLocal 스택에 현재 BulkChange를 저장하고, save() 호출 시 스코프 안이면
// 실제 저장을 지연한다. commit()이 호출되면 비로소 한 번만 저장한다.
//
// 핵심 메커니즘 (BulkChange.java):
//   - private static final ThreadLocal<BulkChange> INSCOPE = new ThreadLocal<>();
//   - BulkChange(Saveable s) { parent = current(); INSCOPE.set(this); }
//   - commit() { pop(); saveable.save(); }
//   - abort() { pop(); }  // 롤백 없이 스코프만 해제
//   - static contains(Saveable s): 스코프 체인을 따라가며 현재 Saveable이 포함되어 있는지 확인
//
// 사용 패턴 (Jenkins 코드의 try-with-resources):
//   try (BulkChange bc = new BulkChange(someObject)) {
//       someObject.setA(...);  // 내부적으로 save() 호출하지만 BulkChange가 가로챔
//       someObject.setB(...);
//       bc.commit();           // 여기서 한 번만 저장
//   }

// goroutine별 BulkChange 스택 관리
// Java의 ThreadLocal에 해당 — Go에서는 goroutine ID를 키로 사용
var (
	bulkChangeMu     sync.Mutex
	bulkChangeScopes = make(map[int64]*BulkChange)
	goroutineIDSeq   int64
)

// goroutineLocalID: goroutine별 고유 ID (시뮬레이션용)
// 실제 Go에서는 goroutine ID를 직접 얻을 수 없으므로 시뮬레이션한다.
// 이 PoC에서는 단일 goroutine에서 실행하므로 고정 ID를 사용한다.
func goroutineLocalID() int64 {
	return 1 // 단일 goroutine 시뮬레이션
}

type BulkChange struct {
	saveable  Saveable
	parent    *BulkChange
	completed bool
}

// NewBulkChange: BulkChange 생성 및 스코프 진입
// Jenkins 코드에서:
//   public BulkChange(Saveable saveable) {
//       this.parent = current();
//       this.saveable = saveable;
//       INSCOPE.set(this);
//   }
func NewBulkChange(s Saveable) *BulkChange {
	bulkChangeMu.Lock()
	defer bulkChangeMu.Unlock()

	gid := goroutineLocalID()
	parent := bulkChangeScopes[gid]
	bc := &BulkChange{
		saveable:  s,
		parent:    parent,
		completed: false,
	}
	bulkChangeScopes[gid] = bc
	fmt.Printf("[BulkChange] 스코프 진입 (saveable=%T)\n", s)
	return bc
}

// Commit: 변경사항 저장 (스코프 해제 후 save() 호출)
// Jenkins 코드에서:
//   public void commit() throws IOException {
//       if (completed) return;
//       completed = true;
//       pop();
//       saveable.save();
//   }
func (bc *BulkChange) Commit() error {
	if bc.completed {
		return nil
	}
	bc.completed = true
	bc.pop()
	fmt.Printf("[BulkChange] 커밋 → save() 호출\n")
	return bc.saveable.Save()
}

// Abort: 스코프 해제 (저장하지 않음)
// Jenkins 코드에서 BulkChange는 close() 메서드가 abort()를 호출한다.
// 실제 트랜잭션과 달리 상태 롤백은 하지 않는다.
func (bc *BulkChange) Abort() {
	if bc.completed {
		return
	}
	bc.completed = true
	bc.pop()
	fmt.Printf("[BulkChange] 중단 (저장하지 않음)\n")
}

// Close: defer로 사용하기 위한 메서드 (Java의 try-with-resources 시뮬레이션)
func (bc *BulkChange) Close() {
	bc.Abort()
}

func (bc *BulkChange) pop() {
	bulkChangeMu.Lock()
	defer bulkChangeMu.Unlock()
	gid := goroutineLocalID()
	bulkChangeScopes[gid] = bc.parent
}

// BulkChangeContains: Saveable이 현재 BulkChange 스코프에 포함되어 있는지 확인
// Jenkins 코드에서:
//   public static boolean contains(Saveable s) {
//       for (BulkChange b = current(); b != null; b = b.parent)
//           if (b.saveable == s || b.saveable == ALL)
//               return true;
//       return false;
//   }
func BulkChangeContains(s Saveable) bool {
	bulkChangeMu.Lock()
	defer bulkChangeMu.Unlock()
	gid := goroutineLocalID()
	for bc := bulkChangeScopes[gid]; bc != nil; bc = bc.parent {
		if bc.saveable == s {
			return true
		}
	}
	return false
}

// ============================================================================
// 6. 데이터 모델 — Jenkins의 XML 영속 대상 구조체
// ============================================================================

// JenkinsConfig: JENKINS_HOME/config.xml에 해당
type JenkinsConfig struct {
	XMLName         xml.Name `xml:"hudson"`
	Version         string   `xml:"version"`
	NumExecutors    int      `xml:"numExecutors"`
	Mode            string   `xml:"mode"`
	UseSecurity     bool     `xml:"useSecurity"`
	SystemMessage   string   `xml:"systemMessage,omitempty"`
	SecurityRealm   string   `xml:"securityRealm,omitempty"`
	AuthzStrategy   string   `xml:"authorizationStrategy,omitempty"`
	Views           []View   `xml:"views>view,omitempty"`
	PrimaryViewName string   `xml:"primaryView,omitempty"`
}

type View struct {
	Name       string   `xml:"name"`
	FilterExec bool     `xml:"filterExecutors"`
	FilterQ    bool     `xml:"filterQueue"`
	Properties string   `xml:"properties,omitempty"`
	JobNames   []string `xml:"jobNames>string,omitempty"`
}

// JobConfig: JENKINS_HOME/jobs/{name}/config.xml에 해당
type JobConfig struct {
	XMLName      xml.Name      `xml:"project"`
	Description  string        `xml:"description"`
	KeepBuilds   *BuildKeeper  `xml:"properties>buildDiscarder,omitempty"`
	SCM          *SCMConfig    `xml:"scm,omitempty"`
	Triggers     []TriggerConf `xml:"triggers>trigger,omitempty"`
	Builders     []BuilderConf `xml:"builders>builder,omitempty"`
	Publishers   []PubConf     `xml:"publishers>publisher,omitempty"`
	Disabled     bool          `xml:"disabled"`
	ConcurrentBuild bool      `xml:"concurrentBuild"`
}

type BuildKeeper struct {
	DaysToKeep int `xml:"daysToKeep"`
	NumToKeep  int `xml:"numToKeep"`
}

type SCMConfig struct {
	Type string `xml:"type,attr"`
	URL  string `xml:"url,omitempty"`
}

type TriggerConf struct {
	Type string `xml:"type,attr"`
	Spec string `xml:"spec"`
}

type BuilderConf struct {
	Type    string `xml:"type,attr"`
	Command string `xml:"command,omitempty"`
}

type PubConf struct {
	Type string `xml:"type,attr"`
	Name string `xml:"name,omitempty"`
}

// BuildRecord: JENKINS_HOME/jobs/{name}/builds/{n}/build.xml에 해당
type BuildRecord struct {
	XMLName       xml.Name    `xml:"build"`
	Number        int         `xml:"number"`
	Result        string      `xml:"result"`
	Duration      int64       `xml:"duration"`
	Timestamp     int64       `xml:"timestamp"`
	BuiltOn       string      `xml:"builtOn,omitempty"`
	Actions       []ActionXml `xml:"actions>action,omitempty"`
	ChangeSet     *ChangeSet  `xml:"changeSet,omitempty"`
	CulpritIds    []string    `xml:"culprits>string,omitempty"`
	KeepLog       bool        `xml:"keepLog"`
	DisplayName   string      `xml:"displayName,omitempty"`
}

type ActionXml struct {
	Type  string `xml:"type,attr,omitempty"`
	Value string `xml:",chardata"`
}

type ChangeSet struct {
	Kind    string      `xml:"kind,attr,omitempty"`
	Entries []ChangeEntry `xml:"entry,omitempty"`
}

type ChangeEntry struct {
	Author  string `xml:"author"`
	Message string `xml:"msg"`
	Path    string `xml:"path,omitempty"`
}

// ============================================================================
// 7. JENKINS_HOME 구조 시뮬레이션
// ============================================================================
// Jenkins의 JENKINS_HOME 디렉토리 구조:
//   JENKINS_HOME/
//   ├── config.xml                    ← JenkinsConfig
//   ├── jobs/
//   │   └── {job-name}/
//   │       ├── config.xml            ← JobConfig
//   │       ├── nextBuildNumber
//   │       └── builds/
//   │           └── {build-number}/
//   │               └── build.xml     ← BuildRecord
//   ├── users/
//   └── plugins/

type JenkinsHome struct {
	RootDir string
}

func NewJenkinsHome(rootDir string) (*JenkinsHome, error) {
	home := &JenkinsHome{RootDir: rootDir}
	// 기본 디렉토리 구조 생성
	dirs := []string{
		rootDir,
		filepath.Join(rootDir, "jobs"),
		filepath.Join(rootDir, "users"),
		filepath.Join(rootDir, "plugins"),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("디렉토리 생성 실패 %s: %w", dir, err)
		}
	}
	return home, nil
}

func (h *JenkinsHome) ConfigFile() *XmlFile {
	return NewXmlFile(filepath.Join(h.RootDir, "config.xml"))
}

func (h *JenkinsHome) JobConfigFile(jobName string) *XmlFile {
	return NewXmlFile(filepath.Join(h.RootDir, "jobs", jobName, "config.xml"))
}

func (h *JenkinsHome) BuildFile(jobName string, buildNumber int) *XmlFile {
	return NewXmlFile(filepath.Join(h.RootDir, "jobs", jobName, "builds",
		fmt.Sprintf("%d", buildNumber), "build.xml"))
}

// ============================================================================
// 8. Saveable 구현체: JenkinsInstance, Job, Build
// ============================================================================

// JenkinsInstance: Jenkins 전체 설정을 관리하는 Saveable 객체
type JenkinsInstance struct {
	Home   *JenkinsHome
	Config JenkinsConfig
	xmlFile *XmlFile
	saveCount int
}

func NewJenkinsInstance(home *JenkinsHome) *JenkinsInstance {
	j := &JenkinsInstance{
		Home: home,
		Config: JenkinsConfig{
			Version:      "2.426",
			NumExecutors: 2,
			Mode:         "NORMAL",
			UseSecurity:  true,
		},
		xmlFile: home.ConfigFile(),
	}
	return j
}

// Save: BulkChange 확인 후 저장
// Jenkins 코드에서의 save() 패턴:
//   public void save() throws IOException {
//       if (BulkChange.contains(this)) return;
//       getConfigFile().write(this);
//       SaveableListener.fireOnChange(this, getConfigFile());
//   }
func (j *JenkinsInstance) Save() error {
	if BulkChangeContains(j) {
		fmt.Printf("[JenkinsInstance] BulkChange 스코프 안 — save() 지연\n")
		return nil
	}
	j.saveCount++
	fmt.Printf("[JenkinsInstance] 저장 중... (save 호출 #%d)\n", j.saveCount)
	if err := j.xmlFile.Write(&j.Config); err != nil {
		return err
	}
	globalListenerRegistry.FireOnChange(j, j.xmlFile)
	return nil
}

func (j *JenkinsInstance) GetXmlFile() *XmlFile {
	return j.xmlFile
}

// SetNumExecutors: Executor 수 변경 (Jenkins의 mutator 패턴)
// 각 mutator는 내부적으로 save()를 호출한다.
func (j *JenkinsInstance) SetNumExecutors(n int) error {
	j.Config.NumExecutors = n
	fmt.Printf("[JenkinsInstance] numExecutors = %d\n", n)
	return j.Save()
}

// SetSystemMessage: 시스템 메시지 변경
func (j *JenkinsInstance) SetSystemMessage(msg string) error {
	j.Config.SystemMessage = msg
	fmt.Printf("[JenkinsInstance] systemMessage = %q\n", msg)
	return j.Save()
}

// SetMode: 모드 변경
func (j *JenkinsInstance) SetMode(mode string) error {
	j.Config.Mode = mode
	fmt.Printf("[JenkinsInstance] mode = %q\n", mode)
	return j.Save()
}

// AddView: 뷰 추가
func (j *JenkinsInstance) AddView(name string) error {
	j.Config.Views = append(j.Config.Views, View{Name: name})
	fmt.Printf("[JenkinsInstance] 뷰 추가: %s\n", name)
	return j.Save()
}

// Job: 잡 설정을 관리하는 Saveable 객체
type Job struct {
	Home    *JenkinsHome
	Name    string
	Config  JobConfig
	xmlFile *XmlFile
	saveCount int
}

func NewJob(home *JenkinsHome, name string) *Job {
	return &Job{
		Home: home,
		Name: name,
		Config: JobConfig{
			Description: name + " job",
		},
		xmlFile: home.JobConfigFile(name),
	}
}

func (j *Job) Save() error {
	if BulkChangeContains(j) {
		fmt.Printf("[Job:%s] BulkChange 스코프 안 — save() 지연\n", j.Name)
		return nil
	}
	j.saveCount++
	fmt.Printf("[Job:%s] 저장 중... (save 호출 #%d)\n", j.Name, j.saveCount)
	if err := j.xmlFile.Write(&j.Config); err != nil {
		return err
	}
	globalListenerRegistry.FireOnChange(j, j.xmlFile)
	return nil
}

func (j *Job) GetXmlFile() *XmlFile {
	return j.xmlFile
}

func (j *Job) SetDescription(desc string) error {
	j.Config.Description = desc
	return j.Save()
}

func (j *Job) SetDisabled(disabled bool) error {
	j.Config.Disabled = disabled
	return j.Save()
}

func (j *Job) AddBuilder(builderType, command string) error {
	j.Config.Builders = append(j.Config.Builders, BuilderConf{
		Type:    builderType,
		Command: command,
	})
	return j.Save()
}

func (j *Job) AddPublisher(pubType, name string) error {
	j.Config.Publishers = append(j.Config.Publishers, PubConf{
		Type: pubType,
		Name: name,
	})
	return j.Save()
}

func (j *Job) AddTrigger(trigType, spec string) error {
	j.Config.Triggers = append(j.Config.Triggers, TriggerConf{
		Type: trigType,
		Spec: spec,
	})
	return j.Save()
}

func (j *Job) SetSCM(scmType, url string) error {
	j.Config.SCM = &SCMConfig{Type: scmType, URL: url}
	return j.Save()
}

func (j *Job) SetBuildDiscarder(daysToKeep, numToKeep int) error {
	j.Config.KeepBuilds = &BuildKeeper{DaysToKeep: daysToKeep, NumToKeep: numToKeep}
	return j.Save()
}

// ============================================================================
// 9. 스키마 진화 (Schema Evolution)
// ============================================================================
// Jenkins의 XmlFile JavaDoc (XmlFile.java 라인 67~118)에서 설명하는 데이터 형식 진화:
//
// (a) 필드 추가: 구 XML에 필드가 없으면 Go zero value로 초기화
//     → encoding/xml은 누락된 필드에 대해 zero value를 유지
//
// (b) 필드 제거: 구 필드를 유지하되 xml:"-" 태그로 무시
//     → transient 키워드 대신 구조체에서 제거하거나 xml:"-" 사용
//
// (c) 필드 변경: 구→신 마이그레이션 (readResolve 패턴)
//     → 역직렬화 후 마이그레이션 함수 호출

// JobConfigV1: 이전 버전 (SCM URL이 별도 필드)
type JobConfigV1 struct {
	XMLName     xml.Name `xml:"project"`
	Description string   `xml:"description"`
	ScmUrl      string   `xml:"scmUrl,omitempty"`  // V1: 단일 문자열
	Disabled    bool     `xml:"disabled"`
}

// JobConfigV2: 새 버전 (SCM이 구조체)
type JobConfigV2 struct {
	XMLName     xml.Name   `xml:"project"`
	Description string     `xml:"description"`
	SCM         *SCMConfig `xml:"scm,omitempty"`    // V2: 구조체
	Disabled    bool       `xml:"disabled"`
	Labels      string     `xml:"labels,omitempty"` // V2: 새 필드
}

// MigrateJobConfig: V1 → V2 마이그레이션
// Jenkins에서는 XStream2.PassthruConverter를 사용하여 마이그레이션한다.
// 구 필드(transient)를 읽고, 새 필드로 변환한 후, 다음 save()에서 새 형식으로 저장.
func MigrateJobConfig(v1 *JobConfigV1) *JobConfigV2 {
	v2 := &JobConfigV2{
		Description: v1.Description,
		Disabled:    v1.Disabled,
	}
	// V1의 scmUrl → V2의 SCM 구조체로 변환
	if v1.ScmUrl != "" {
		v2.SCM = &SCMConfig{
			Type: "git",
			URL:  v1.ScmUrl,
		}
		fmt.Printf("[마이그레이션] scmUrl=%q → SCM{type=git, url=%q}\n", v1.ScmUrl, v1.ScmUrl)
	}
	return v2
}

// ============================================================================
// 10. 데모 함수들
// ============================================================================

func printSeparator(title string) {
	fmt.Println()
	fmt.Println(strings.Repeat("=", 72))
	fmt.Printf("  %s\n", title)
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println()
}

// demoAtomicFileWriter: AtomicFileWriter의 원자적 쓰기 시연
func demoAtomicFileWriter(tmpDir string) {
	printSeparator("데모 1: AtomicFileWriter — 원자적 파일 쓰기")

	targetPath := filepath.Join(tmpDir, "test-atomic.xml")

	// 1. 초기 파일 생성
	fmt.Println("[단계 1] 초기 파일 생성")
	writer, err := NewAtomicFileWriter(targetPath)
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}
	writer.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	writer.WriteString("<config><version>1.0</version></config>\n")
	if err := writer.Commit(); err != nil {
		fmt.Printf("  커밋 오류: %v\n", err)
		return
	}
	content, _ := os.ReadFile(targetPath)
	fmt.Printf("  파일 내용:\n%s\n", string(content))

	// 2. 성공적인 업데이트
	fmt.Println("[단계 2] 원자적 업데이트 (성공)")
	writer, _ = NewAtomicFileWriter(targetPath)
	writer.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	writer.WriteString("<config><version>2.0</version><updated>true</updated></config>\n")
	writer.Commit()
	content, _ = os.ReadFile(targetPath)
	fmt.Printf("  업데이트된 내용:\n%s\n", string(content))

	// 3. 실패한 업데이트 (abort)
	fmt.Println("[단계 3] 원자적 업데이트 (실패 → abort)")
	writer, _ = NewAtomicFileWriter(targetPath)
	writer.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	writer.WriteString("<config><version>BROKEN</version></config>\n")
	fmt.Println("  시뮬레이션: 쓰기 중 오류 발생 → abort() 호출")
	writer.Abort()
	content, _ = os.ReadFile(targetPath)
	fmt.Printf("  파일 내용 (원본 유지):\n%s\n", string(content))

	// 4. 동시 쓰기 안전성 검증
	fmt.Println("[단계 4] 동시 쓰기 안전성 검증")
	var wg sync.WaitGroup
	successCount := 0
	var mu sync.Mutex

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			w, err := NewAtomicFileWriter(targetPath)
			if err != nil {
				return
			}
			w.WriteString(fmt.Sprintf("<?xml version=\"1.0\"?>\n<config><writer>%d</writer></config>\n", n))
			if err := w.Commit(); err == nil {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	content, _ = os.ReadFile(targetPath)
	fmt.Printf("  %d개 goroutine 완료, 최종 파일 내용:\n%s\n", successCount, string(content))
	fmt.Println("  → 파일이 항상 유효한 상태 (깨지지 않음)")
}

// demoXmlFile: XmlFile의 read/write 기능 시연
func demoXmlFile(tmpDir string) {
	printSeparator("데모 2: XmlFile — XML 데이터 파일 관리")

	// 1. JenkinsConfig 저장
	fmt.Println("[단계 1] JenkinsConfig 저장")
	configFile := NewXmlFile(filepath.Join(tmpDir, "config.xml"))
	config := &JenkinsConfig{
		Version:       "2.426.3",
		NumExecutors:  4,
		Mode:          "EXCLUSIVE",
		UseSecurity:   true,
		SystemMessage: "Jenkins CI 서버에 오신 것을 환영합니다",
		SecurityRealm: "hudsonPrivateSecurityRealm",
		AuthzStrategy: "fullControlOnceLoggedIn",
		Views: []View{
			{Name: "All", FilterExec: false, FilterQ: false},
			{Name: "Build Jobs", FilterExec: true},
		},
		PrimaryViewName: "All",
	}
	if err := configFile.Write(config); err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}
	content, _ := configFile.AsString()
	fmt.Println("  저장된 XML:")
	fmt.Println(indentContent(content, "    "))

	// 2. JenkinsConfig 읽기
	fmt.Println("[단계 2] JenkinsConfig 읽기")
	loaded := &JenkinsConfig{}
	if err := configFile.Read(loaded); err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}
	fmt.Printf("  Version: %s\n", loaded.Version)
	fmt.Printf("  NumExecutors: %d\n", loaded.NumExecutors)
	fmt.Printf("  Mode: %s\n", loaded.Mode)
	fmt.Printf("  SystemMessage: %s\n", loaded.SystemMessage)
	fmt.Printf("  Views: %d개\n", len(loaded.Views))

	// 3. JobConfig 저장
	fmt.Println("\n[단계 3] JobConfig 저장")
	jobFile := NewXmlFile(filepath.Join(tmpDir, "jobs", "my-project", "config.xml"))
	jobConfig := &JobConfig{
		Description: "프로젝트 빌드 잡",
		KeepBuilds:  &BuildKeeper{DaysToKeep: 30, NumToKeep: 100},
		SCM:         &SCMConfig{Type: "git", URL: "https://github.com/example/project.git"},
		Triggers: []TriggerConf{
			{Type: "TimerTrigger", Spec: "H/15 * * * *"},
		},
		Builders: []BuilderConf{
			{Type: "Shell", Command: "mvn clean install"},
			{Type: "Shell", Command: "make test"},
		},
		Publishers: []PubConf{
			{Type: "JUnitResultArchiver", Name: "target/surefire-reports/*.xml"},
			{Type: "Mailer", Name: "dev@example.com"},
		},
		Disabled:        false,
		ConcurrentBuild: true,
	}
	if err := jobFile.Write(jobConfig); err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}
	content, _ = jobFile.AsString()
	fmt.Println("  저장된 XML:")
	fmt.Println(indentContent(content, "    "))

	// 4. BuildRecord 저장
	fmt.Println("[단계 4] BuildRecord 저장")
	buildFile := NewXmlFile(filepath.Join(tmpDir, "jobs", "my-project", "builds", "42", "build.xml"))
	buildRecord := &BuildRecord{
		Number:    42,
		Result:    "SUCCESS",
		Duration:  125340,
		Timestamp: time.Now().UnixMilli(),
		BuiltOn:   "built-in",
		Actions: []ActionXml{
			{Type: "CauseAction", Value: "Started by timer"},
		},
		ChangeSet: &ChangeSet{
			Kind: "git",
			Entries: []ChangeEntry{
				{Author: "developer", Message: "Fix null pointer exception", Path: "src/Main.java"},
			},
		},
		KeepLog: false,
	}
	if err := buildFile.Write(buildRecord); err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}
	content, _ = buildFile.AsString()
	fmt.Println("  저장된 XML:")
	fmt.Println(indentContent(content, "    "))

	// 5. 파일 존재 및 삭제
	fmt.Println("[단계 5] 파일 존재 확인 및 삭제")
	fmt.Printf("  config.xml 존재: %v\n", configFile.Exists())
	fmt.Printf("  jobConfig 존재: %v\n", jobFile.Exists())
	fmt.Printf("  buildRecord 존재: %v\n", buildFile.Exists())
}

// demoBulkChange: BulkChange 트랜잭션 패턴 시연
func demoBulkChange(tmpDir string) {
	printSeparator("데모 3: BulkChange — 다중 변경 트랜잭션 패턴")

	home, _ := NewJenkinsHome(filepath.Join(tmpDir, "bulk-test"))
	jenkins := NewJenkinsInstance(home)

	// 1. BulkChange 없이 — 매 변경마다 save() 호출
	fmt.Println("[단계 1] BulkChange 없이 변경 (매번 save)")
	jenkins.saveCount = 0
	jenkins.SetNumExecutors(4)
	jenkins.SetSystemMessage("Hello")
	jenkins.SetMode("EXCLUSIVE")
	fmt.Printf("  → save() 호출 횟수: %d (매번 저장됨)\n\n", jenkins.saveCount)

	// 2. BulkChange 사용 — commit()까지 save() 지연
	fmt.Println("[단계 2] BulkChange 사용 (commit까지 지연)")
	jenkins.saveCount = 0
	bc := NewBulkChange(jenkins)
	// Java에서는 try-with-resources, Go에서는 defer
	defer bc.Close()

	jenkins.SetNumExecutors(8)
	jenkins.SetSystemMessage("Bulk updated")
	jenkins.SetMode("NORMAL")
	jenkins.AddView("Pipeline")
	jenkins.AddView("Deploy")

	fmt.Printf("  → commit 전 save() 호출 횟수: %d (지연됨)\n", jenkins.saveCount)

	bc.Commit()
	fmt.Printf("  → commit 후 save() 호출 횟수: %d (한 번만 저장)\n\n", jenkins.saveCount)

	// 3. BulkChange 중첩
	fmt.Println("[단계 3] BulkChange 중첩 (스코프 체인)")
	jenkins.saveCount = 0

	bc1 := NewBulkChange(jenkins)
	jenkins.SetNumExecutors(16)

	job := NewJob(home, "nested-test")
	bc2 := NewBulkChange(job)
	job.SetDescription("Nested bulk change test")
	job.SetDisabled(false)
	job.AddBuilder("Shell", "echo hello")

	fmt.Printf("  → 중첩 BulkChange 안: jenkins.saveCount=%d, job.saveCount=%d\n",
		jenkins.saveCount, job.saveCount)

	bc2.Commit()
	fmt.Printf("  → 내부 BulkChange 커밋 후: job.saveCount=%d\n", job.saveCount)

	bc1.Commit()
	fmt.Printf("  → 외부 BulkChange 커밋 후: jenkins.saveCount=%d\n", jenkins.saveCount)

	// 4. BulkChange abort — 변경사항 저장하지 않음
	fmt.Println("\n[단계 4] BulkChange 중단 (abort)")
	jenkins.saveCount = 0
	bcAbort := NewBulkChange(jenkins)
	jenkins.SetNumExecutors(999)
	jenkins.SetSystemMessage("This will be aborted")
	fmt.Printf("  → abort 전 save() 호출 횟수: %d (지연됨)\n", jenkins.saveCount)
	bcAbort.Abort()
	fmt.Printf("  → abort 후: save()가 호출되지 않음 (변경은 메모리에 남지만 저장 안 됨)\n")

	// BulkChange 밖에서의 동작 확인
	jenkins.saveCount = 0
	jenkins.SetNumExecutors(4)
	fmt.Printf("  → BulkChange 밖: save() 호출 횟수: %d (즉시 저장)\n", jenkins.saveCount)
}

// demoSchemaEvolution: 데이터 형식 진화 시연
func demoSchemaEvolution(tmpDir string) {
	printSeparator("데모 4: 스키마 진화 (Schema Evolution)")

	// 1. 필드 추가: 기존 XML에 새 필드가 없는 경우
	fmt.Println("[단계 1] 필드 추가 — 구 XML에 새 필드 없음")
	oldXml := `<?xml version="1.0" encoding="UTF-8"?>
<project>
  <description>Old job</description>
  <disabled>false</disabled>
</project>`

	oldFile := filepath.Join(tmpDir, "schema-v1.xml")
	os.WriteFile(oldFile, []byte(oldXml), 0644)

	xf := NewXmlFile(oldFile)
	newConfig := &JobConfigV2{}
	xf.Read(newConfig)
	fmt.Printf("  구 XML에서 읽은 결과:\n")
	fmt.Printf("    Description: %q\n", newConfig.Description)
	fmt.Printf("    Disabled: %v\n", newConfig.Disabled)
	fmt.Printf("    SCM: %v (nil = 구 XML에 없는 필드 → zero value)\n", newConfig.SCM)
	fmt.Printf("    Labels: %q (빈 문자열 = 기본값)\n\n", newConfig.Labels)

	// 2. 필드 제거: 새 구조체에서 제거된 필드는 무시됨
	fmt.Println("[단계 2] 필드 제거 — 새 구조체에서 제거된 필드 무시")
	extraFieldXml := `<?xml version="1.0" encoding="UTF-8"?>
<project>
  <description>Job with extra fields</description>
  <oldField>this field is deprecated</oldField>
  <anotherOldField>also deprecated</anotherOldField>
  <disabled>true</disabled>
</project>`

	extraFile := filepath.Join(tmpDir, "schema-extra.xml")
	os.WriteFile(extraFile, []byte(extraFieldXml), 0644)

	xf2 := NewXmlFile(extraFile)
	cleanConfig := &JobConfigV2{}
	xf2.Read(cleanConfig)
	fmt.Printf("  구 XML에서 읽은 결과 (unknown 필드 무시):\n")
	fmt.Printf("    Description: %q\n", cleanConfig.Description)
	fmt.Printf("    Disabled: %v\n", cleanConfig.Disabled)
	fmt.Printf("    oldField: (무시됨 — 구조체에 해당 필드 없음)\n\n")

	// 새 형식으로 저장 → 구 필드는 제거됨
	outFile := NewXmlFile(filepath.Join(tmpDir, "schema-clean.xml"))
	outFile.Write(cleanConfig)
	content, _ := outFile.AsString()
	fmt.Printf("  새 형식으로 저장 (구 필드 제거됨):\n%s\n", indentContent(content, "    "))

	// 3. 필드 변경: V1 → V2 마이그레이션
	fmt.Println("[단계 3] 필드 변경 — V1 → V2 마이그레이션")
	v1Xml := `<?xml version="1.0" encoding="UTF-8"?>
<project>
  <description>Legacy job with scmUrl</description>
  <scmUrl>https://github.com/legacy/repo.git</scmUrl>
  <disabled>false</disabled>
</project>`

	v1File := filepath.Join(tmpDir, "schema-v1-migrate.xml")
	os.WriteFile(v1File, []byte(v1Xml), 0644)

	xfV1 := NewXmlFile(v1File)
	v1Config := &JobConfigV1{}
	xfV1.Read(v1Config)
	fmt.Printf("  V1 형식에서 읽은 결과:\n")
	fmt.Printf("    Description: %q\n", v1Config.Description)
	fmt.Printf("    ScmUrl: %q\n\n", v1Config.ScmUrl)

	// 마이그레이션 실행
	v2Config := MigrateJobConfig(v1Config)
	fmt.Printf("  마이그레이션 결과 (V2):\n")
	fmt.Printf("    Description: %q\n", v2Config.Description)
	fmt.Printf("    SCM.Type: %q\n", v2Config.SCM.Type)
	fmt.Printf("    SCM.URL: %q\n\n", v2Config.SCM.URL)

	// V2 형식으로 저장
	v2File := NewXmlFile(filepath.Join(tmpDir, "schema-v2-migrated.xml"))
	v2File.Write(v2Config)
	v2Content, _ := v2File.AsString()
	fmt.Printf("  V2 형식으로 저장:\n%s\n", indentContent(v2Content, "    "))
}

// demoSaveableListener: SaveableListener 이벤트 시연
func demoSaveableListener(tmpDir string) {
	printSeparator("데모 5: SaveableListener — 저장 이벤트 리스너")

	listener := &LoggingSaveableListener{}
	globalListenerRegistry.Register(listener)

	home, _ := NewJenkinsHome(filepath.Join(tmpDir, "listener-test"))
	jenkins := NewJenkinsInstance(home)

	// 1. 직접 저장 → 리스너 호출됨
	fmt.Println("[단계 1] 직접 save() → 리스너 호출")
	jenkins.Save()

	// 2. Job 생성 및 저장
	fmt.Println("\n[단계 2] Job 생성 및 저장")
	job := NewJob(home, "listener-demo-job")
	job.SetDescription("리스너 테스트용 잡")
	job.AddBuilder("Shell", "echo test")

	// 3. 리스너 이벤트 요약
	fmt.Printf("\n[단계 3] 총 %d개의 리스너 이벤트 발생\n", len(listener.events))
	for i, evt := range listener.events {
		fmt.Printf("  %d) %s\n", i+1, evt)
	}
}

// demoJenkinsHome: JENKINS_HOME 디렉토리 구조 시연
func demoJenkinsHome(tmpDir string) {
	printSeparator("데모 6: JENKINS_HOME 디렉토리 구조 시뮬레이션")

	home, _ := NewJenkinsHome(filepath.Join(tmpDir, "jenkins-home"))

	// config.xml 저장
	jenkins := NewJenkinsInstance(home)
	jenkins.Config.Views = []View{
		{Name: "All"},
		{Name: "Build Jobs", FilterExec: true, JobNames: []string{"project-a", "project-b"}},
	}
	jenkins.Config.PrimaryViewName = "All"
	jenkins.Save()

	// 여러 Job 생성
	jobNames := []string{"web-app", "api-server", "batch-job"}
	for _, name := range jobNames {
		job := NewJob(home, name)
		job.SetDescription(fmt.Sprintf("%s 빌드 잡", name))
		job.AddBuilder("Shell", fmt.Sprintf("./build-%s.sh", name))
		job.AddPublisher("JUnitResultArchiver", "reports/*.xml")
		job.AddTrigger("TimerTrigger", "H/10 * * * *")
		job.Save()

		// 빌드 레코드 저장
		for i := 1; i <= 3; i++ {
			buildFile := home.BuildFile(name, i)
			result := "SUCCESS"
			if i == 2 && name == "batch-job" {
				result = "FAILURE"
			}
			record := &BuildRecord{
				Number:    i,
				Result:    result,
				Duration:  int64(30000 + i*10000),
				Timestamp: time.Now().Add(-time.Duration(3-i) * time.Hour).UnixMilli(),
				BuiltOn:   "built-in",
			}
			buildFile.Write(record)
		}
	}

	// 디렉토리 구조 출력
	fmt.Println("[결과] JENKINS_HOME 구조:")
	printDirTree(home.RootDir, "", true)
}

// demoCompleteWorkflow: 전체 워크플로우 통합 시연
func demoCompleteWorkflow(tmpDir string) {
	printSeparator("데모 7: 전체 워크플로우 통합 시연")

	home, _ := NewJenkinsHome(filepath.Join(tmpDir, "workflow"))

	// 1. Jenkins 인스턴스 초기 설정 (BulkChange 사용)
	fmt.Println("[단계 1] Jenkins 초기 설정 (BulkChange)")
	jenkins := NewJenkinsInstance(home)
	bc := NewBulkChange(jenkins)
	jenkins.Config.NumExecutors = 8
	jenkins.Config.Mode = "EXCLUSIVE"
	jenkins.Config.SystemMessage = "Production CI Server"
	jenkins.Config.UseSecurity = true
	jenkins.Config.SecurityRealm = "ldap"
	jenkins.Config.AuthzStrategy = "projectMatrix"
	jenkins.Config.Views = []View{
		{Name: "All"},
		{Name: "Deployments", FilterExec: true},
	}
	jenkins.Config.PrimaryViewName = "All"
	bc.Commit()
	fmt.Printf("  ✓ 설정 저장 완료 (save 1회)\n")

	// 2. Job 생성 (BulkChange 사용)
	fmt.Println("\n[단계 2] Job 생성 (BulkChange)")
	job := NewJob(home, "production-deploy")
	bc2 := NewBulkChange(job)
	job.Config.Description = "프로덕션 배포 잡"
	job.Config.ConcurrentBuild = false
	job.Config.KeepBuilds = &BuildKeeper{DaysToKeep: 90, NumToKeep: 50}
	job.Config.SCM = &SCMConfig{Type: "git", URL: "https://github.com/company/service.git"}
	job.Config.Triggers = []TriggerConf{
		{Type: "GitHubPush", Spec: ""},
	}
	job.Config.Builders = []BuilderConf{
		{Type: "Shell", Command: "mvn clean package -DskipTests=false"},
		{Type: "Shell", Command: "docker build -t service:latest ."},
		{Type: "Shell", Command: "kubectl apply -f k8s/deployment.yaml"},
	}
	job.Config.Publishers = []PubConf{
		{Type: "JUnitResultArchiver", Name: "target/surefire-reports/*.xml"},
		{Type: "Mailer", Name: "ops@company.com"},
		{Type: "SlackNotifier", Name: "#deploy-notifications"},
	}
	bc2.Commit()
	fmt.Printf("  ✓ Job 설정 저장 완료 (save 1회)\n")

	// 3. 빌드 실행 시뮬레이션
	fmt.Println("\n[단계 3] 빌드 실행 시뮬레이션")
	for i := 1; i <= 5; i++ {
		buildFile := home.BuildFile("production-deploy", i)
		results := []string{"SUCCESS", "SUCCESS", "FAILURE", "UNSTABLE", "SUCCESS"}
		durations := []int64{180000, 195000, 45000, 210000, 175000}
		record := &BuildRecord{
			Number:    i,
			Result:    results[i-1],
			Duration:  durations[i-1],
			Timestamp: time.Now().Add(-time.Duration(5-i) * time.Hour).UnixMilli(),
			BuiltOn:   "built-in",
			ChangeSet: &ChangeSet{
				Kind: "git",
				Entries: []ChangeEntry{
					{
						Author:  fmt.Sprintf("dev%d", i),
						Message: fmt.Sprintf("Feature #%d implementation", 100+i),
					},
				},
			},
		}
		buildFile.Write(record)
		fmt.Printf("  빌드 #%d: %s (%dms)\n", i, results[i-1], durations[i-1])
	}

	// 4. 저장된 데이터 읽기 검증
	fmt.Println("\n[단계 4] 저장된 데이터 읽기 검증")
	loadedConfig := &JenkinsConfig{}
	home.ConfigFile().Read(loadedConfig)
	fmt.Printf("  config.xml → NumExecutors=%d, Mode=%s\n",
		loadedConfig.NumExecutors, loadedConfig.Mode)

	loadedJob := &JobConfig{}
	home.JobConfigFile("production-deploy").Read(loadedJob)
	fmt.Printf("  job/config.xml → Builders=%d개, Publishers=%d개\n",
		len(loadedJob.Builders), len(loadedJob.Publishers))

	loadedBuild := &BuildRecord{}
	home.BuildFile("production-deploy", 3).Read(loadedBuild)
	fmt.Printf("  builds/3/build.xml → Result=%s, Duration=%dms\n",
		loadedBuild.Result, loadedBuild.Duration)

	// 5. 최종 디렉토리 구조
	fmt.Println("\n[단계 5] 최종 JENKINS_HOME 구조")
	printDirTree(home.RootDir, "", true)
}

// ============================================================================
// 유틸리티 함수
// ============================================================================

func indentContent(content, prefix string) string {
	lines := strings.Split(content, "\n")
	var result []string
	for _, line := range lines {
		if line != "" {
			result = append(result, prefix+line)
		}
	}
	return strings.Join(result, "\n")
}

// printDirTree: 디렉토리 트리 출력 (Jenkins의 ls -R 시뮬레이션)
func printDirTree(path string, prefix string, isRoot bool) {
	if isRoot {
		fmt.Printf("  %s/\n", filepath.Base(path))
		prefix = "  "
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return
	}

	for i, entry := range entries {
		isLast := i == len(entries)-1
		connector := "├── "
		childPrefix := "│   "
		if isLast {
			connector = "└── "
			childPrefix = "    "
		}

		if entry.IsDir() {
			fmt.Printf("%s%s%s/\n", prefix, connector, entry.Name())
			printDirTree(filepath.Join(path, entry.Name()), prefix+childPrefix, false)
		} else {
			info, _ := entry.Info()
			size := int64(0)
			if info != nil {
				size = info.Size()
			}
			fmt.Printf("%s%s%s (%d bytes)\n", prefix, connector, entry.Name(), size)
		}
	}
}

// ============================================================================
// 메인 함수
// ============================================================================

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Jenkins PoC-09: XML 영속성 시뮬레이션 (XStream/XmlFile)            ║")
	fmt.Println("║                                                                      ║")
	fmt.Println("║  참조: jenkins/core/src/main/java/hudson/XmlFile.java                ║")
	fmt.Println("║        jenkins/core/src/main/java/hudson/util/AtomicFileWriter.java  ║")
	fmt.Println("║        jenkins/core/src/main/java/hudson/BulkChange.java             ║")
	fmt.Println("║        jenkins/core/src/main/java/hudson/model/Saveable.java         ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════╝")

	// 임시 디렉토리 생성
	tmpDir, err := os.MkdirTemp("", "jenkins-poc09-*")
	if err != nil {
		fmt.Printf("임시 디렉토리 생성 실패: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)
	fmt.Printf("\n작업 디렉토리: %s\n", tmpDir)

	// 데모 실행
	demoAtomicFileWriter(tmpDir)
	demoXmlFile(tmpDir)
	demoBulkChange(tmpDir)
	demoSchemaEvolution(tmpDir)
	demoSaveableListener(tmpDir)
	demoJenkinsHome(tmpDir)
	demoCompleteWorkflow(tmpDir)

	printSeparator("요약: Jenkins XML 영속성 시스템의 핵심 설계")
	fmt.Println(`  Jenkins의 XML 영속성 시스템은 다음 핵심 원칙으로 설계되었다:

  1. AtomicFileWriter (원자적 쓰기)
     - 임시 파일에 먼저 기록 → rename으로 원자적 교체
     - 크래시 시에도 파일이 깨지지 않음 (fsync + atomic move)
     - 실제 코드: hudson.util.AtomicFileWriter

  2. XmlFile (XML 데이터 파일 관리)
     - read(): XStream.fromXML() → 객체 역직렬화
     - write(): AtomicFileWriter 사용 → 안전한 저장
     - 실제 코드: hudson.XmlFile

  3. BulkChange (트랜잭션 패턴)
     - ThreadLocal 스택으로 여러 save() 호출을 commit()까지 지연
     - 성능 최적화: 여러 변경사항을 한 번의 I/O로 처리
     - 실제 코드: hudson.BulkChange

  4. Schema Evolution (데이터 형식 진화)
     - 필드 추가: zero value 초기화 (XStream의 기본 동작)
     - 필드 제거: transient 키워드로 직렬화 제외
     - 필드 변경: 구 필드 → 신 필드 마이그레이션 (readResolve 패턴)

  5. SaveableListener (저장 이벤트 리스너)
     - onChange(): 저장 후 알림 (감사 로그, 변경 추적 등)
     - @Extension으로 자동 등록 (ExtensionPoint 시스템)
     - 실제 코드: hudson.model.listeners.SaveableListener

  6. JENKINS_HOME 구조
     - config.xml: 전역 설정
     - jobs/{name}/config.xml: 잡별 설정
     - jobs/{name}/builds/{n}/build.xml: 빌드별 레코드
     - 모든 것이 XML 파일 — 파일 시스템이 곧 데이터베이스`)

	fmt.Println("\n프로그램이 정상 종료되었습니다.")
}
