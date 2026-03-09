// Package main은 Jenkins Console Log/Annotation 시스템의 핵심 개념을 시뮬레이션한다.
//
// Jenkins의 콘솔 어노테이션 시스템은 다음 핵심 메커니즘으로 동작한다:
// 1. ConsoleNote: 직렬화 가능한 메타데이터를 PREAMBLE/POSTAMBLE로 감싸 로그에 삽입
// 2. ConsoleAnnotator: 라인별 상태 기계로 HTML 마크업 생성
// 3. AnnotatedLargeText: 로그 읽기 시 어노테이션을 HTML로 변환
// 4. HMAC 서명으로 보안 보장
//
// 이 PoC는 Go 표준 라이브러리만으로 이 전체 파이프라인을 재현한다.
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"time"
)

// =============================================================================
// 1. ConsoleNote 시뮬레이션
// =============================================================================

// PREAMBLE과 POSTAMBLE: ANSI 이스케이프 시퀀스로 터미널에서 숨김
const (
	PREAMBLE  = "\033[8mha:" // ANSI 숨김(concealed) 모드 + 매직 문자열
	POSTAMBLE = "\033[0m"    // ANSI 리셋
)

// hmacKey는 ConsoleNote 서명에 사용되는 비밀 키 (Jenkins에서는 $JENKINS_HOME/secrets/에 저장)
var hmacKey = []byte("jenkins-console-note-mac-secret-key")

// NoteType은 ConsoleNote의 종류를 나타낸다
type NoteType string

const (
	NoteHyperlink  NoteType = "hyperlink"
	NoteError      NoteType = "error"
	NoteWarning    NoteType = "warning"
	NoteExpandable NoteType = "expandable"
	NoteMojo       NoteType = "mojo"
)

// ConsoleNote는 콘솔 출력의 특정 위치에 부착되는 메타데이터 객체
// Jenkins 원본: core/src/main/java/hudson/console/ConsoleNote.java
type ConsoleNote struct {
	Type    NoteType `json:"type"`    // 노트 종류
	URL     string   `json:"url"`     // 하이퍼링크 URL (NoteHyperlink 전용)
	Length  int      `json:"length"`  // 적용할 텍스트 길이
	Message string   `json:"message"` // 추가 메시지
}

// encode는 ConsoleNote를 인코딩된 문자열로 변환한다
// Jenkins 원본: ConsoleNote.encodeToBytes()
// 흐름: JSON 직렬화 → GZIP 압축 → HMAC 서명 → Base64 인코딩 → PREAMBLE/POSTAMBLE 감싸기
func (n *ConsoleNote) encode() (string, error) {
	// 1단계: JSON 직렬화 (Jenkins는 Java ObjectOutputStream 사용)
	jsonData, err := json.Marshal(n)
	if err != nil {
		return "", fmt.Errorf("JSON 직렬화 실패: %w", err)
	}

	// 2단계: GZIP 압축
	var compressed bytes.Buffer
	gzWriter := gzip.NewWriter(&compressed)
	if _, err := gzWriter.Write(jsonData); err != nil {
		return "", fmt.Errorf("GZIP 압축 실패: %w", err)
	}
	gzWriter.Close()

	compressedBytes := compressed.Bytes()

	// 3단계: HMAC-SHA256 서명 생성
	mac := hmac.New(sha256.New, hmacKey)
	mac.Write(compressedBytes)
	macBytes := mac.Sum(nil)

	// 4단계: 바이너리 데이터 구성
	// [MAC 길이(음수 int32)][MAC 데이터][압축 데이터 크기(int32)][압축 데이터]
	var payload bytes.Buffer
	binary.Write(&payload, binary.BigEndian, int32(-len(macBytes))) // 음수: 새 형식 표시
	payload.Write(macBytes)
	binary.Write(&payload, binary.BigEndian, int32(len(compressedBytes)))
	payload.Write(compressedBytes)

	// 5단계: Base64 인코딩
	encoded := base64.StdEncoding.EncodeToString(payload.Bytes())

	// 6단계: PREAMBLE + 인코딩된 데이터 + POSTAMBLE
	return PREAMBLE + encoded + POSTAMBLE, nil
}

// readFrom은 인코딩된 ConsoleNote를 역직렬화한다
// Jenkins 원본: ConsoleNote.readFrom()
func readFrom(data string) (*ConsoleNote, error) {
	// 1단계: PREAMBLE/POSTAMBLE 제거
	if !strings.HasPrefix(data, PREAMBLE) || !strings.HasSuffix(data, POSTAMBLE) {
		return nil, fmt.Errorf("유효하지 않은 ConsoleNote 형식")
	}
	encoded := data[len(PREAMBLE) : len(data)-len(POSTAMBLE)]

	// 2단계: Base64 디코딩
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("Base64 디코딩 실패: %w", err)
	}

	reader := bytes.NewReader(decoded)

	// 3단계: MAC 길이 읽기 (음수 int32)
	var macSzNeg int32
	if err := binary.Read(reader, binary.BigEndian, &macSzNeg); err != nil {
		return nil, fmt.Errorf("MAC 길이 읽기 실패: %w", err)
	}
	macSz := int(-macSzNeg)
	if macSz < 0 || macSz > len(decoded) {
		return nil, fmt.Errorf("잘못된 MAC 길이: %d (데이터 변조 의심)", macSz)
	}

	// 4단계: MAC 데이터 읽기
	macData := make([]byte, macSz)
	reader.Read(macData)

	// 5단계: 압축 데이터 크기 읽기
	var dataSz int32
	if err := binary.Read(reader, binary.BigEndian, &dataSz); err != nil {
		return nil, fmt.Errorf("데이터 크기 읽기 실패: %w", err)
	}
	if dataSz < 0 || int(dataSz) > len(decoded) {
		return nil, fmt.Errorf("잘못된 데이터 크기: %d (데이터 변조 의심)", dataSz)
	}

	// 6단계: 압축 데이터 읽기
	compressedData := make([]byte, dataSz)
	reader.Read(compressedData)

	// 7단계: HMAC 검증
	mac := hmac.New(sha256.New, hmacKey)
	mac.Write(compressedData)
	expectedMAC := mac.Sum(nil)
	if !hmac.Equal(macData, expectedMAC) {
		return nil, fmt.Errorf("HMAC 검증 실패: ConsoleNote가 변조되었거나 서명되지 않음")
	}

	// 8단계: GZIP 해제
	gzReader, err := gzip.NewReader(bytes.NewReader(compressedData))
	if err != nil {
		return nil, fmt.Errorf("GZIP 해제 실패: %w", err)
	}
	defer gzReader.Close()

	var jsonData bytes.Buffer
	jsonData.ReadFrom(gzReader)

	// 9단계: JSON 역직렬화
	var note ConsoleNote
	if err := json.Unmarshal(jsonData.Bytes(), &note); err != nil {
		return nil, fmt.Errorf("JSON 역직렬화 실패: %w", err)
	}

	return &note, nil
}

// =============================================================================
// 2. ConsoleAnnotator 시뮬레이션
// =============================================================================

// ConsoleAnnotator는 콘솔 라인을 어노테이트하는 상태 기계
// Jenkins 원본: core/src/main/java/hudson/console/ConsoleAnnotator.java
type ConsoleAnnotator interface {
	// Annotate는 한 줄을 어노테이트하고, 다음 줄을 위한 어노테이터를 반환
	// nil 반환 시 더 이상 어노테이션하지 않음
	Annotate(text *MarkupText) ConsoleAnnotator
}

// MarkupText는 텍스트에 HTML 마크업을 추가하는 구조체
// Jenkins 원본: core/src/main/java/hudson/MarkupText.java
type MarkupText struct {
	Text    string
	markups []markup
}

type markup struct {
	start int
	end   int
	open  string
	close string
}

func NewMarkupText(text string) *MarkupText {
	return &MarkupText{Text: text}
}

func (mt *MarkupText) AddMarkup(start, end int, open, close string) {
	if start >= 0 && end <= len(mt.Text) && start <= end {
		mt.markups = append(mt.markups, markup{start, end, open, close})
	}
}

// ToHTML는 마크업이 적용된 HTML 문자열을 반환
func (mt *MarkupText) ToHTML() string {
	if len(mt.markups) == 0 {
		return html.EscapeString(mt.Text)
	}

	// 마크업을 위치 순으로 적용 (단순화: 중첩 미처리)
	result := ""
	lastIdx := 0
	for _, m := range mt.markups {
		if m.start > lastIdx {
			result += html.EscapeString(mt.Text[lastIdx:m.start])
		}
		result += m.open + html.EscapeString(mt.Text[m.start:m.end]) + m.close
		lastIdx = m.end
	}
	if lastIdx < len(mt.Text) {
		result += html.EscapeString(mt.Text[lastIdx:])
	}
	return result
}

// =============================================================================
// 3. 구체적 어노테이터 구현
// =============================================================================

// HyperlinkAnnotator는 URL을 하이퍼링크로 변환하는 어노테이터
// Jenkins 원본: core/src/main/java/hudson/console/HyperlinkNote.java
type HyperlinkAnnotator struct {
	URL    string
	Length int
}

func (a *HyperlinkAnnotator) Annotate(text *MarkupText) ConsoleAnnotator {
	text.AddMarkup(0, a.Length,
		fmt.Sprintf("<a href='%s'>", html.EscapeString(a.URL)), "</a>")
	return nil // 한 줄만 어노테이트
}

// ErrorAnnotator는 에러 라인을 강조하는 어노테이터
// Jenkins 원본: core/src/main/java/hudson/tasks/_maven/MavenErrorNote.java
type ErrorAnnotator struct{}

func (a *ErrorAnnotator) Annotate(text *MarkupText) ConsoleAnnotator {
	text.AddMarkup(0, len(text.Text),
		"<span style='color:red;font-weight:bold'>", "</span>")
	return nil
}

// WarningAnnotator는 경고 라인을 강조하는 어노테이터
type WarningAnnotator struct{}

func (a *WarningAnnotator) Annotate(text *MarkupText) ConsoleAnnotator {
	text.AddMarkup(0, len(text.Text),
		"<span style='color:orange'>", "</span>")
	return nil
}

// MojoAnnotator는 Maven Mojo 실행 정보를 강조
type MojoAnnotator struct{}

func (a *MojoAnnotator) Annotate(text *MarkupText) ConsoleAnnotator {
	text.AddMarkup(0, len(text.Text),
		"<b style='color:blue'>", "</b>")
	return nil
}

// =============================================================================
// 4. ConsoleAnnotationOutputStream 시뮬레이션
// =============================================================================

// ConsoleAnnotationOutputStream은 콘솔 출력을 라인별로 처리하면서
// ConsoleNote를 추출하고 HTML로 변환한다
// Jenkins 원본: core/src/main/java/hudson/console/ConsoleAnnotationOutputStream.java
type ConsoleAnnotationOutputStream struct {
	annotator ConsoleAnnotator
}

// ProcessLine은 한 줄의 콘솔 출력을 처리한다
func (s *ConsoleAnnotationOutputStream) ProcessLine(line string) string {
	// 1단계: 라인에서 ConsoleNote 찾기 및 추출
	notes, cleanLine := extractNotes(line)

	// 2단계: MarkupText 생성
	mt := NewMarkupText(cleanLine)

	// 3단계: ConsoleNote에서 어노테이터 생성 및 적용
	for _, note := range notes {
		var ann ConsoleAnnotator
		switch note.Type {
		case NoteHyperlink:
			ann = &HyperlinkAnnotator{URL: note.URL, Length: note.Length}
		case NoteError:
			ann = &ErrorAnnotator{}
		case NoteWarning:
			ann = &WarningAnnotator{}
		case NoteMojo:
			ann = &MojoAnnotator{}
		}
		if ann != nil {
			newAnn := ann.Annotate(mt)
			if newAnn != nil {
				s.annotator = newAnn // 다음 줄에 적용할 어노테이터 저장
			}
		}
	}

	// 4단계: 기존 상태 어노테이터 적용
	if s.annotator != nil {
		s.annotator = s.annotator.Annotate(mt)
	}

	return mt.ToHTML()
}

// extractNotes는 라인에서 인코딩된 ConsoleNote를 추출하고 순수 텍스트를 반환
func extractNotes(line string) ([]*ConsoleNote, string) {
	var notes []*ConsoleNote
	cleanLine := line

	for {
		pIdx := strings.Index(cleanLine, PREAMBLE)
		if pIdx < 0 {
			break
		}
		eIdx := strings.Index(cleanLine[pIdx:], POSTAMBLE)
		if eIdx < 0 {
			break
		}
		eIdx += pIdx

		// 인코딩된 노트 추출
		encodedNote := cleanLine[pIdx : eIdx+len(POSTAMBLE)]

		// 노트 역직렬화
		note, err := readFrom(encodedNote)
		if err != nil {
			fmt.Printf("  [역직렬화 실패: %s]\n", err)
		} else {
			notes = append(notes, note)
		}

		// 노트를 라인에서 제거
		cleanLine = cleanLine[:pIdx] + cleanLine[eIdx+len(POSTAMBLE):]
	}

	return notes, cleanLine
}

// removeNotes는 라인에서 ConsoleNote 바이너리를 단순 문자열 치환으로 제거
// Jenkins 원본: ConsoleNote.removeNotes()
func removeNotes(line string) string {
	for {
		idx := strings.Index(line, PREAMBLE)
		if idx < 0 {
			return line
		}
		e := strings.Index(line[idx:], POSTAMBLE)
		if e < 0 {
			return line
		}
		e += idx
		line = line[:idx] + line[e+len(POSTAMBLE):]
	}
}

// =============================================================================
// 5. ConsoleLogFilter 시뮬레이션
// =============================================================================

// ConsoleLogFilter는 출력 스트림에 필터를 적용한다
// Jenkins 원본: core/src/main/java/hudson/console/ConsoleLogFilter.java
type ConsoleLogFilter interface {
	Filter(line string) string
}

// PasswordMaskFilter는 비밀번호를 마스킹하는 필터
type PasswordMaskFilter struct {
	Secrets []string
}

func (f *PasswordMaskFilter) Filter(line string) string {
	result := line
	for _, secret := range f.Secrets {
		result = strings.ReplaceAll(result, secret, "****")
	}
	return result
}

// TimestampFilter는 타임스탬프를 추가하는 필터
type TimestampFilter struct{}

func (f *TimestampFilter) Filter(line string) string {
	ts := time.Now().Format("15:04:05.000")
	return fmt.Sprintf("[%s] %s", ts, line)
}

// =============================================================================
// 메인 데모
// =============================================================================

func main() {
	fmt.Println("=== Jenkins Console Log/Annotation 시스템 시뮬레이션 ===")
	fmt.Println()

	// ─────────────────────────────────────────────────
	// 데모 1: ConsoleNote 인코딩/디코딩
	// ─────────────────────────────────────────────────
	fmt.Println("--- 1. ConsoleNote 인코딩/디코딩 ---")

	note := &ConsoleNote{
		Type:   NoteHyperlink,
		URL:    "/job/my-project/42/",
		Length: 13,
	}

	encoded, err := note.encode()
	if err != nil {
		fmt.Printf("인코딩 실패: %s\n", err)
		return
	}

	fmt.Printf("원본 노트: Type=%s, URL=%s, Length=%d\n", note.Type, note.URL, note.Length)
	fmt.Printf("인코딩 결과 길이: %d bytes\n", len(encoded))
	fmt.Printf("PREAMBLE 시작: %q\n", encoded[:len(PREAMBLE)])
	fmt.Printf("POSTAMBLE 끝:  %q\n", encoded[len(encoded)-len(POSTAMBLE):])

	decoded, err := readFrom(encoded)
	if err != nil {
		fmt.Printf("디코딩 실패: %s\n", err)
		return
	}

	fmt.Printf("디코딩 결과: Type=%s, URL=%s, Length=%d\n", decoded.Type, decoded.URL, decoded.Length)
	fmt.Printf("HMAC 검증: 성공\n")
	fmt.Println()

	// ─────────────────────────────────────────────────
	// 데모 2: 변조 감지
	// ─────────────────────────────────────────────────
	fmt.Println("--- 2. HMAC 변조 감지 ---")

	// 인코딩된 데이터를 일부 변조
	tampered := encoded[:len(PREAMBLE)+5] + "X" + encoded[len(PREAMBLE)+6:]
	_, err = readFrom(tampered)
	if err != nil {
		fmt.Printf("변조된 노트 감지: %s\n", err)
	}
	fmt.Println()

	// ─────────────────────────────────────────────────
	// 데모 3: 빌드 콘솔 출력 시뮬레이션
	// ─────────────────────────────────────────────────
	fmt.Println("--- 3. 빌드 콘솔 출력 생성 (Producer) ---")

	// ConsoleLogFilter 적용
	filters := []ConsoleLogFilter{
		&PasswordMaskFilter{Secrets: []string{"s3cr3tP@ss"}},
		&TimestampFilter{},
	}

	// 빌드 로그 라인 생성 (어노테이션 포함)
	var buildLog []string

	// 일반 텍스트
	buildLog = append(buildLog, "Started by user admin")

	// 하이퍼링크 어노테이션이 포함된 라인
	hlNote := &ConsoleNote{Type: NoteHyperlink, URL: "/job/my-project/42/", Length: 13}
	hlEncoded, _ := hlNote.encode()
	buildLog = append(buildLog, hlEncoded+"Build #42 > Console Output")

	// Maven Mojo 어노테이션
	mojoNote := &ConsoleNote{Type: NoteMojo, Length: 50}
	mojoEncoded, _ := mojoNote.encode()
	buildLog = append(buildLog, mojoEncoded+"[INFO] --- maven-compiler-plugin:3.11.0:compile ---")

	// 일반 Maven 출력
	buildLog = append(buildLog, "[INFO] Compiling 42 source files")
	buildLog = append(buildLog, "[INFO] Connecting with password: s3cr3tP@ss")

	// 경고 어노테이션
	warnNote := &ConsoleNote{Type: NoteWarning, Length: 60}
	warnEncoded, _ := warnNote.encode()
	buildLog = append(buildLog, warnEncoded+"[WARNING] Some dependencies are not resolved correctly")

	// 에러 어노테이션
	errNote := &ConsoleNote{Type: NoteError, Length: 40}
	errEncoded, _ := errNote.encode()
	buildLog = append(buildLog, errEncoded+"[ERROR] Build failed with exit code 1")

	buildLog = append(buildLog, "Finished: FAILURE")

	// 필터 적용
	fmt.Println("\n[필터 적용 후 로그 (원시 형태 - 노트 제거)]:")
	for _, line := range buildLog {
		filtered := line
		for _, f := range filters {
			filtered = f.Filter(filtered)
		}
		// 노트를 제거하고 순수 텍스트만 표시
		clean := removeNotes(filtered)
		fmt.Printf("  %s\n", clean)
	}

	// ─────────────────────────────────────────────────
	// 데모 4: HTML 렌더링 (Consumer)
	// ─────────────────────────────────────────────────
	fmt.Println("\n--- 4. HTML 렌더링 (Consumer - ConsoleAnnotationOutputStream) ---")

	stream := &ConsoleAnnotationOutputStream{}

	fmt.Println("\n[어노테이션 적용된 HTML 출력]:")
	for i, line := range buildLog {
		// 먼저 필터 적용
		filtered := line
		for _, f := range filters {
			filtered = f.Filter(filtered)
		}

		// HTML 렌더링
		htmlOutput := stream.ProcessLine(filtered)
		fmt.Printf("  라인 %d: %s\n", i+1, htmlOutput)
	}

	// ─────────────────────────────────────────────────
	// 데모 5: 바이너리 포맷 분석
	// ─────────────────────────────────────────────────
	fmt.Println("\n--- 5. ConsoleNote 바이너리 포맷 분석 ---")

	simpleNote := &ConsoleNote{Type: NoteError, Length: 10, Message: "test"}
	simpleEncoded, _ := simpleNote.encode()

	fmt.Printf("전체 길이: %d bytes\n", len(simpleEncoded))
	fmt.Printf("PREAMBLE (%d bytes): %q\n", len(PREAMBLE), PREAMBLE)
	fmt.Printf("POSTAMBLE (%d bytes): %q\n", len(POSTAMBLE), POSTAMBLE)
	fmt.Printf("Base64 페이로드 길이: %d chars\n",
		len(simpleEncoded)-len(PREAMBLE)-len(POSTAMBLE))
	fmt.Printf("터미널에서 볼 때: 노트는 ANSI 숨김 모드로 보이지 않음\n")

	fmt.Println("\n=== 시뮬레이션 완료 ===")
}
