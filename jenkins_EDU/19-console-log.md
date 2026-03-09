# 19. Console Log / Annotation 시스템 Deep-Dive

## 1. 개요

Jenkins의 콘솔 출력(Console Output) 시스템은 빌드 실행 중 생성되는 텍스트 로그를
단순히 저장하고 표시하는 것을 넘어, **구조화된 어노테이션(annotation)**을 통해
풍부한 HTML 마크업을 오버레이할 수 있는 정교한 아키텍처를 갖추고 있다.

### 왜(Why) 이 서브시스템이 존재하는가?

CI/CD 빌드 로그는 본질적으로 **비구조화된 텍스트 스트림**이다. 그러나 사용자에게는
다음과 같은 니즈가 있다:

1. **하이퍼링크**: 빌드 로그 속 URL, Jenkins 내부 경로를 클릭 가능한 링크로 표시
2. **에러/경고 강조**: Maven 빌드의 에러/경고 메시지를 시각적으로 구분
3. **섹션 접기**: 긴 로그를 논리적 섹션으로 나누어 접을 수 있는 UI
4. **보안**: 비밀번호나 토큰이 로그에 노출되지 않도록 마스킹
5. **증분 로딩**: 브라우저에서 실시간으로 로그를 스트리밍

단순 정규식 매칭으로는 이 모든 요구를 충족하기 어렵다. Jenkins는 **로그 생산 시점**에
메타데이터를 직렬화하여 삽입하고, **로그 소비 시점**에 이를 HTML로 변환하는
이중 구조를 채택했다.

## 2. 핵심 아키텍처

```
┌─────────────────────────────────────────────────────────────┐
│                    빌드 실행 (Producer)                       │
│                                                             │
│  stdout/stderr ──→ ConsoleLogFilter ──→ OutputStream        │
│                          │                    │             │
│                    ConsoleNote.encodeTo()      │             │
│                          │                    │             │
│                    ┌─────▼────────────────────▼─────┐       │
│                    │    log 파일 (텍스트 + 인코딩된   │       │
│                    │    ConsoleNote 바이너리)        │       │
│                    └────────────────────────────────┘       │
└─────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────┐
│                    웹 UI (Consumer)                          │
│                                                             │
│  AnnotatedLargeText                                         │
│       │                                                     │
│       ├─→ ConsoleAnnotationOutputStream                     │
│       │        │                                            │
│       │        ├─→ ConsoleNote.readFrom() (역직렬화)         │
│       │        │        │                                   │
│       │        │   ConsoleAnnotator.annotate()               │
│       │        │        │                                   │
│       │        └─→ MarkupText → HTML 출력                    │
│       │                                                     │
│       └─→ X-ConsoleAnnotator 헤더 (상태 전달)                │
└─────────────────────────────────────────────────────────────┘
```

## 3. 핵심 클래스 분석

### 3.1 ConsoleNote — 어노테이션의 핵심

**경로**: `core/src/main/java/hudson/console/ConsoleNote.java`

`ConsoleNote<T>`는 콘솔 출력의 특정 위치에 부착되는 **직렬화 가능한 데이터 객체**이다.

```java
public abstract class ConsoleNote<T> implements Serializable,
    Describable<ConsoleNote<?>>, ExtensionPoint {

    // 어노테이션 처리: 컨텍스트와 텍스트를 받아 HTML 마크업 추가
    public abstract ConsoleAnnotator annotate(T context, MarkupText text, int charPos);

    // 직렬화 → GZIP → Base64 인코딩 → ANSI 이스케이프 시퀀스로 감싸기
    public void encodeTo(OutputStream out) throws IOException {
        out.write(encodeToBytes().toByteArray());
    }
}
```

#### 인코딩 프로세스

`encodeToBytes()` 메서드가 핵심 인코딩을 수행한다:

```
ConsoleNote 객체
    ↓ ObjectOutputStream (Java 직렬화)
    ↓ GZIPOutputStream (압축)
    ↓ HMAC 서명 생성 (보안)
    ↓ Base64 인코딩
    ↓ PREAMBLE + 데이터 + POSTAMBLE로 감싸기
```

```java
private ByteArrayOutputStream encodeToBytes() throws IOException {
    ByteArrayOutputStream buf = new ByteArrayOutputStream();
    try (OutputStream gzos = new GZIPOutputStream(buf);
         ObjectOutputStream oos = ...) {
        oos.writeObject(this);
    }
    ByteArrayOutputStream buf2 = new ByteArrayOutputStream();
    try (DataOutputStream dos = new DataOutputStream(Base64.getEncoder().wrap(buf2))) {
        buf2.write(PREAMBLE);           // "\u001B[8mha:"
        byte[] mac = MAC.mac(buf.toByteArray());
        dos.writeInt(-mac.length);       // 음수: 새 형식 표시
        dos.write(mac);                  // HMAC 서명
        dos.writeInt(buf.size());        // 데이터 크기
        buf.writeTo(dos);               // 압축된 직렬화 데이터
    }
    buf2.write(POSTAMBLE);              // "\u001B[0m"
    return buf2;
}
```

#### PREAMBLE / POSTAMBLE

```java
public static final String PREAMBLE_STR = "\u001B[8mha:";  // ANSI 숨김 시작 + 매직
public static final String POSTAMBLE_STR = "\u001B[0m";     // ANSI 리셋
```

**왜 ANSI 이스케이프 시퀀스인가?**

- `\u001B[8m`: ANSI "숨김(concealed)" 모드 — 터미널에서 직접 볼 때 어노테이션 데이터가 보이지 않음
- `\u001B[0m`: ANSI 리셋 — 이후 텍스트가 정상 출력
- 사람이 `cat`으로 로그를 볼 때 어노테이션 바이너리가 화면에 쓰레기로 나타나지 않도록 하는 배려

#### HMAC 보안 (SECURITY-382)

```java
private static final HMACConfidentialKey MAC =
    new HMACConfidentialKey(ConsoleNote.class, "MAC");
```

- ConsoleNote는 **역직렬화 공격**에 취약할 수 있다
- 마스터 JVM에서 생성한 노트만 유효한 HMAC 서명을 가진다
- 에이전트에서 생성된 서명 없는 노트는 `readFrom()`에서 거부됨
- `INSECURE` 플래그로 레거시 호환성 제공 가능 (비권장)

### 3.2 ConsoleAnnotator — 상태 기계

**경로**: `core/src/main/java/hudson/console/ConsoleAnnotator.java`

콘솔 출력은 **라인 단위**로 처리되며, `ConsoleAnnotator<T>`는 이 처리의 **상태 기계**를 모델링한다.

```java
public abstract class ConsoleAnnotator<T> implements Serializable {
    // 한 줄을 어노테이트하고, 다음 줄을 위한 새 어노테이터 반환
    public abstract ConsoleAnnotator<T> annotate(T context, MarkupText text);
}
```

핵심 패턴은 **체이닝**이다:

```
ca.annotate(context, line1)  → ca2
ca2.annotate(context, line2) → ca3
...
```

#### ConsoleAnnotatorAggregator

여러 어노테이터를 하나로 합치는 내부 클래스:

```java
private static final class ConsoleAnnotatorAggregator<T> extends ConsoleAnnotator<T> {
    List<ConsoleAnnotator<T>> list;

    @Override
    public ConsoleAnnotator annotate(T context, MarkupText text) {
        ListIterator<ConsoleAnnotator<T>> itr = list.listIterator();
        while (itr.hasNext()) {
            ConsoleAnnotator a = itr.next();
            ConsoleAnnotator b = a.annotate(context, text);
            if (a != b) {
                if (b == null) itr.remove();
                else           itr.set(b);
            }
        }
        return switch (list.size()) {
            case 0 -> null;
            case 1 -> list.getFirst();
            default -> this;
        };
    }
}
```

### 3.3 ConsoleAnnotationOutputStream — 변환 엔진

**경로**: `core/src/main/java/hudson/console/ConsoleAnnotationOutputStream.java`

`LineTransformationOutputStream`을 상속하여 바이트 스트림을 라인 단위로 처리하면서
ConsoleNote를 추출하고 HTML 마크업으로 변환한다.

```java
public class ConsoleAnnotationOutputStream<T> extends LineTransformationOutputStream {
    private final Writer out;
    private final T context;
    private ConsoleAnnotator<T> ann;

    @Override
    protected void eol(byte[] in, int sz) throws IOException {
        // 1. 라인에서 ConsoleNote PREAMBLE 찾기
        int next = ConsoleNote.findPreamble(in, 0, sz);

        List<ConsoleAnnotator<T>> annotators = null;

        // 2. 바이트 → 문자 변환하면서 ConsoleNote 추출
        int written = 0;
        while (next >= 0) {
            lineOut.write(in, written, next - written);
            lineOut.flush();

            final int charPos = strBuf.length();
            ConsoleNote a = ConsoleNote.readFrom(new DataInputStream(...));
            if (a != null) {
                annotators.add(/* charPos 위치의 어노테이터 */);
            }
            // ...
            next = ConsoleNote.findPreamble(in, written, sz - written);
        }

        // 3. MarkupText에 어노테이션 적용
        MarkupText mt = new MarkupText(strBuf.toString());
        if (ann != null) ann = ann.annotate(context, mt);

        // 4. HTML로 출력 (이스케이핑 포함)
        out.write(mt.toString(true));
    }
}
```

### 3.4 AnnotatedLargeText — 진입점

**경로**: `core/src/main/java/hudson/console/AnnotatedLargeText.java`

`LargeText`를 확장하여 콘솔 어노테이션 기능을 추가한다.
HTTP 요청을 처리하면서 `ConsoleAnnotator` 상태를 클라이언트와 주고받는다.

```java
public class AnnotatedLargeText<T> extends LargeText {
    private T context;

    // 상태 생성/복원
    private ConsoleAnnotator<T> createAnnotator(StaplerRequest2 req) {
        String base64 = req.getHeader("X-ConsoleAnnotator");
        if (base64 != null) {
            // 암호화된 어노테이터 상태 복원
            Cipher sym = PASSING_ANNOTATOR.decrypt();
            // 역직렬화 + 1시간 이내 타임스탬프 검증 (리플레이 공격 방지)
        }
        return ConsoleAnnotator.initial(context);
    }

    // HTML 렌더링
    public long writeHtmlTo(long start, Writer w) throws IOException {
        ConsoleAnnotationOutputStream<T> caw = new ConsoleAnnotationOutputStream<>(
            w, createAnnotator(req), context, charset);
        long r = super.writeLogTo(start, caw);

        // 어노테이터 상태를 암호화하여 응답 헤더로 전달
        String state = Base64.getEncoder().encodeToString(baos.toByteArray());
        rsp.setHeader("X-ConsoleAnnotator", state);
        return r;
    }
}
```

#### X-ConsoleAnnotator 상태 전달

```
클라이언트 → 서버: GET /job/foo/42/consoleText
                   X-ConsoleAnnotator: <암호화된 이전 상태>

서버 → 클라이언트: 200 OK
                   X-ConsoleAnnotator: <암호화된 새 상태>
                   Content-Type: text/html

                   <어노테이트된 HTML 콘텐츠>
```

**왜 암호화하는가?**

- `ConsoleAnnotator`는 `Serializable` — 악성 클라이언트가 조작된 바이트를 보내면
  임의 객체 역직렬화 공격이 가능
- `CryptoConfidentialKey`로 대칭키 암호화하여 방지
- 타임스탬프 검증으로 1시간 이상 된 상태 거부 (리플레이 공격 방지)

### 3.5 ConsoleLogFilter — 출력 필터

**경로**: `core/src/main/java/hudson/console/ConsoleLogFilter.java`

빌드 콘솔 출력 스트림에 필터를 삽입할 수 있는 확장 포인트.

```java
public abstract class ConsoleLogFilter implements ExtensionPoint {
    // 빌드별 로거 데코레이션
    public OutputStream decorateLogger(Run build, OutputStream logger)
        throws IOException, InterruptedException;

    // 에이전트 통신용 로거 데코레이션
    public OutputStream decorateLogger(@NonNull Computer computer, OutputStream logger)
        throws IOException, InterruptedException;
}
```

**대표적 활용 사례**:
- 비밀번호 마스킹 (예: credentials-binding 플러그인)
- 타임스탬프 추가 (예: timestamper 플러그인)
- ANSI 컬러 코드 처리

### 3.6 HyperlinkNote — 대표적 ConsoleNote 구현

**경로**: `core/src/main/java/hudson/console/HyperlinkNote.java`

```java
public class HyperlinkNote extends ConsoleNote {
    private final String url;
    private final int length;

    @Override
    public ConsoleAnnotator annotate(Object context, MarkupText text, int charPos) {
        String url = this.url;
        if (url.startsWith("/")) {
            // Jenkins 내부 경로 → 컨텍스트 경로 추가
            url = req.getContextPath() + url;
        }
        text.addMarkup(charPos, charPos + length,
            "<a href='" + Util.escape(url) + "'>", "</a>");
        return null;
    }

    // 편의 메서드: URL + 텍스트를 인코딩된 문자열로
    public static String encodeTo(String url, String text) {
        return new HyperlinkNote(url, text.length()).encode() + text;
    }
}
```

## 4. 데이터 흐름 상세

### 4.1 빌드 실행 시 (쓰기 경로)

```mermaid
sequenceDiagram
    participant Build as 빌드 프로세스
    participant Filter as ConsoleLogFilter
    participant Note as ConsoleNote
    participant File as 로그 파일

    Build->>Filter: stdout/stderr 출력
    Filter->>Filter: 비밀번호 마스킹 등

    Note->>Note: encode() 호출
    Note->>Note: 직렬화 → GZIP → HMAC → Base64
    Note->>File: PREAMBLE + 인코딩된 데이터 + POSTAMBLE

    Build->>File: 일반 텍스트 출력
```

### 4.2 웹 UI 표시 시 (읽기 경로)

```mermaid
sequenceDiagram
    participant Browser as 브라우저
    participant ALT as AnnotatedLargeText
    participant CAOS as ConsoleAnnotationOutputStream
    participant CN as ConsoleNote

    Browser->>ALT: GET /consoleText (X-ConsoleAnnotator 헤더)
    ALT->>ALT: createAnnotator() - 이전 상태 복원
    ALT->>CAOS: writeLogTo(start, caw)

    loop 각 라인
        CAOS->>CAOS: eol() - 라인 끝 감지
        CAOS->>CN: findPreamble() + readFrom()
        CN->>CN: Base64 디코딩 → HMAC 검증 → GZIP 해제 → 역직렬화
        CN->>CAOS: annotate(context, text, charPos)
        CAOS->>CAOS: MarkupText → HTML
    end

    ALT->>Browser: HTML + X-ConsoleAnnotator (새 상태)
```

## 5. Maven 콘솔 어노테이션 예시

**경로**: `core/src/main/java/hudson/tasks/_maven/`

Jenkins에 내장된 Maven 빌드 어노테이터:

| 클래스 | 설명 |
|--------|------|
| `MavenConsoleAnnotator` | Maven 출력 패턴 매칭 어노테이터 |
| `MavenMojoNote` | `[INFO] --- maven-compiler-plugin:...` 패턴 강조 |
| `Maven3MojoNote` | Maven 3 형식 Mojo 실행 강조 |
| `MavenErrorNote` | `[ERROR]` 라인 빨간색 강조 |
| `MavenWarningNote` | `[WARNING]` 라인 노란색 강조 |

## 6. PlainTextConsoleOutputStream — 어노테이션 제거

**경로**: `core/src/main/java/hudson/console/PlainTextConsoleOutputStream.java`

REST API나 CLI로 로그를 가져올 때는 어노테이션이 불필요하다.
`PlainTextConsoleOutputStream`은 PREAMBLE~POSTAMBLE 사이의 바이너리를
모두 제거하여 순수 텍스트만 출력한다.

```java
// AnnotatedLargeText.writeLogTo(long, OutputStream)
@Override
public long writeLogTo(long start, OutputStream out) throws IOException {
    return super.writeLogTo(start, new PlainTextConsoleOutputStream(out));
}
```

## 7. 확장 포인트 정리

| 확장 포인트 | 역할 | 등록 방식 |
|-------------|------|----------|
| `ConsoleNote` | 특정 위치에 메타데이터 삽입 | `@Extension` |
| `ConsoleAnnotator` | 라인별 마크업 생성 (상태 기계) | `ConsoleNote.annotate()` 반환값 |
| `ConsoleAnnotatorFactory` | 패턴 매칭 기반 어노테이터 생성 | `@Extension` |
| `ConsoleLogFilter` | 출력 스트림 필터링 (마스킹 등) | `@Extension` |
| `ConsoleAnnotationDescriptor` | ConsoleNote의 디스크립터 | `@Extension` |

## 8. 보안 고려사항

### 8.1 직렬화 보안

```
[생성] Jenkins 마스터 → ConsoleNote → HMAC 서명 + 직렬화 → 로그 파일
[검증] 로그 읽기 → HMAC 검증 → 서명 유효하면 역직렬화 허용
```

- HMAC 키는 `$JENKINS_HOME/secrets/` 에 저장
- 에이전트에서 생성된 노트에는 서명이 없어 무시됨
- `ClassFilter.DEFAULT`로 허용된 클래스만 역직렬화 가능

### 8.2 어노테이터 상태 보안

```
[요청] 브라우저 → X-ConsoleAnnotator: <암호화된 상태>
[검증] 서버 → CryptoConfidentialKey로 복호화 → 타임스탬프 1시간 이내 확인
```

## 9. ConsoleNote 바이너리 포맷

```
┌──────────────────────────────────────────────────────┐
│ PREAMBLE (7 bytes): \x1B[8mha:                       │
├──────────────────────────────────────────────────────┤
│ Base64 인코딩 영역:                                   │
│   ├── MAC 길이 (4 bytes, 음수 int)                   │
│   ├── MAC 데이터 (N bytes, HMAC-SHA256)               │
│   ├── 압축 데이터 크기 (4 bytes, int)                  │
│   └── GZIP 압축된 ObjectOutputStream 데이터            │
├──────────────────────────────────────────────────────┤
│ POSTAMBLE (4 bytes): \x1B[0m                         │
└──────────────────────────────────────────────────────┘
```

## 10. ConsoleNote.removeNotes() — 정적 유틸리티

```java
public static String removeNotes(String line) {
    while (true) {
        int idx = line.indexOf(PREAMBLE_STR);
        if (idx < 0)  return line;
        int e = line.indexOf(POSTAMBLE_STR, idx);
        if (e < 0)    return line;
        line = line.substring(0, idx) + line.substring(e + POSTAMBLE_STR.length());
    }
}
```

PREAMBLE과 POSTAMBLE 사이의 모든 데이터를 단순 문자열 치환으로 제거한다.

## 11. 성능 고려사항

| 항목 | 접근 방식 | 이유 |
|------|----------|------|
| 라인 버퍼 | `LineBuffer` (64KB 초과시 재생성) | 메모리 누수 방지 |
| Base64 | `java.util.Base64` (Java 8+) | 외부 의존성 제거 |
| 증분 로딩 | `LargeText` 기반 offset 추적 | 대용량 로그 효율적 처리 |
| GZIP 압축 | 직렬화 데이터 압축 | 로그 파일 크기 최소화 |

## 12. 설정 옵션

| 시스템 프로퍼티 | 설명 | 기본값 |
|----------------|------|--------|
| `hudson.console.ConsoleNote.INSECURE` | 서명 없는 노트 허용 | `false` |

## 13. 정리

Jenkins Console Log/Annotation 시스템의 설계 핵심은 **생산자-소비자 분리**이다.
로그를 생성하는 쪽(빌드, 플러그인)은 `ConsoleNote`로 풍부한 메타데이터를 삽입하고,
로그를 소비하는 쪽(웹 UI)은 `ConsoleAnnotator`로 이를 HTML로 변환한다.
이 과정에서 HMAC 서명으로 보안을 보장하고, ANSI 이스케이프 시퀀스로 터미널 호환성을 유지하며,
암호화된 상태 전달로 증분 로딩을 지원한다.

이 아키텍처 덕분에 수백 가지 플러그인이 각자의 방식으로 콘솔 출력을 풍부하게
만들 수 있으며, 동시에 보안과 성능을 유지할 수 있다.
