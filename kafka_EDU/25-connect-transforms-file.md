# 25. Connect Transforms & File Connector

> Kafka Connect의 SMT(Single Message Transforms) 프레임워크와 File Connector 레퍼런스 구현

---

## 1. 개요

Kafka Connect는 커넥터와 브로커 사이에서 메시지를 변환하는 SMT 프레임워크를 제공한다.
이 문서는 두 가지를 다룬다:

1. **Connect Transforms** (`connect/transforms/src/`): 내장 SMT 구현체들
2. **File Connector** (`connect/file/src/`): 가장 단순한 레퍼런스 커넥터 구현

### 왜 SMT가 필요한가?

ETL 파이프라인에서 소스와 싱크의 스키마가 다를 때, 별도의 변환 서비스 없이
Connect 파이프라인 내에서 메시지를 변환할 수 있다.
SMT는 경량 변환에 적합하며, 복잡한 변환은 Kafka Streams를 사용한다.

---

## 2. SMT 아키텍처

### 2.1 변환 체인

```
┌──────────────────────────────────────────────────────────┐
│                    Connect Pipeline                       │
│                                                          │
│  ┌──────┐   ┌──────┐   ┌──────┐   ┌──────┐   ┌──────┐  │
│  │Source │──▶│SMT #1│──▶│SMT #2│──▶│SMT #3│──▶│ Sink │  │
│  │Record │   │Cast  │   │Insert│   │Filter│   │Record│  │
│  └──────┘   │      │   │Field │   │      │   └──────┘  │
│              └──────┘   └──────┘   └──────┘             │
└──────────────────────────────────────────────────────────┘
```

### 2.2 Transformation 인터페이스

```java
public interface Transformation<R extends ConnectRecord<R>>
        extends Configurable, Closeable {
    R apply(R record);       // 레코드 변환
    ConfigDef config();      // 설정 정의
    void close();            // 리소스 정리
    void configure(Map<String, ?> configs);  // 설정 적용
}
```

### 2.3 Key/Value 분리 패턴

```java
// Cast.java — Key/Value 서브클래스 패턴
public abstract class Cast<R extends ConnectRecord<R>>
        implements Transformation<R> {

    protected abstract Schema operatingSchema(R record);
    protected abstract Object operatingValue(R record);
    protected abstract R newRecord(R record, Schema updatedSchema,
                                   Object updatedValue);

    public static final class Key<R extends ConnectRecord<R>> extends Cast<R> {
        @Override
        protected Schema operatingSchema(R record) {
            return record.keySchema();  // 키에 적용
        }
        @Override
        protected Object operatingValue(R record) {
            return record.key();
        }
    }

    public static final class Value<R extends ConnectRecord<R>> extends Cast<R> {
        @Override
        protected Schema operatingSchema(R record) {
            return record.valueSchema();  // 값에 적용
        }
        @Override
        protected Object operatingValue(R record) {
            return record.value();
        }
    }
}
```

**왜 Key/Value를 분리하는가?**

동일한 변환 로직을 레코드의 키와 값에 독립적으로 적용할 수 있다.
예: 키는 String→Int64로, 값은 Float64→Int32로 캐스팅.

---

## 3. 내장 Transform 상세

### 3.1 Cast — 타입 캐스팅

**소스**: `connect/transforms/src/main/java/org/apache/kafka/connect/transforms/Cast.java`

```java
public R apply(R record) {
    if (operatingValue(record) == null) return record;

    if (operatingSchema(record) == null) {
        return applySchemaless(record);  // Map 기반
    } else {
        return applyWithSchema(record);  // Struct 기반
    }
}
```

#### 지원 타입 변환

| 입력 타입 | 출력 가능 타입 |
|----------|--------------|
| INT8, INT16, INT32, INT64 | 모든 숫자, BOOLEAN, STRING |
| FLOAT32, FLOAT64 | 모든 숫자, BOOLEAN, STRING |
| BOOLEAN | 모든 숫자, STRING |
| STRING | 모든 숫자, BOOLEAN |
| BYTES | STRING (Base64) |

#### 캐스팅 구현

```java
private static Object castValueToType(Schema schema, Object value,
                                       Schema.Type targetType) {
    if (value == null) return null;

    // 논리 타입(Date, Time, Timestamp)은 내부 표현으로 변환
    if (schema != null && schema.name() != null && targetType != Type.STRING) {
        value = encodeLogicalType(schema, value);
    }

    return switch (targetType) {
        case INT8    -> castToInt8(value);
        case INT16   -> castToInt16(value);
        case INT32   -> castToInt32(value);
        case INT64   -> castToInt64(value);
        case FLOAT32 -> castToFloat32(value);
        case FLOAT64 -> castToFloat64(value);
        case BOOLEAN -> castToBoolean(value);
        case STRING  -> castToString(value);
        default -> throw new DataException("Unsupported: " + targetType);
    };
}
```

#### 스키마 캐싱

```java
private Cache<Schema, Schema> schemaUpdateCache;

@Override
public void configure(Map<String, ?> props) {
    schemaUpdateCache = new SynchronizedCache<>(new LRUCache<>(16));
}

private Schema getOrBuildSchema(Schema valueSchema) {
    Schema updatedSchema = schemaUpdateCache.get(valueSchema);
    if (updatedSchema != null) return updatedSchema;
    // 스키마 빌드 후 캐시에 저장
    updatedSchema = builder.build();
    schemaUpdateCache.put(valueSchema, updatedSchema);
    return updatedSchema;
}
```

**왜 스키마를 캐싱하는가?**

동일한 입력 스키마에 대해 매번 새 스키마를 빌드하면 GC 압력이 증가한다.
LRU 캐시(크기 16)로 최근 사용한 스키마를 재활용한다.

### 3.2 Filter — 레코드 필터링

```java
public class Filter<R extends ConnectRecord<R>>
        implements Transformation<R> {

    @Override
    public R apply(R record) {
        return null;  // null 반환 = 레코드 드롭
    }
}
```

**왜 Filter가 null을 반환하는가?**

Connect 런타임은 SMT 체인에서 null이 반환되면 해당 레코드를 버린다.
Filter는 Predicate와 함께 사용하여 조건부 필터링을 구현한다:

```json
{
  "transforms": "dropNull",
  "transforms.dropNull.type": "org.apache.kafka.connect.transforms.Filter",
  "transforms.dropNull.predicate": "isNull",
  "predicates": "isNull",
  "predicates.isNull.type": "org.apache.kafka.connect.transforms.predicates.RecordIsTombstone"
}
```

### 3.3 InsertField — 필드 삽입

```java
public abstract class InsertField<R extends ConnectRecord<R>>
        implements Transformation<R> {
    // 레코드 메타데이터에서 필드를 추출하여 삽입
    // topic.field: Kafka 토픽 이름
    // partition.field: 파티션 번호
    // offset.field: 오프셋 (sink only)
    // timestamp.field: 레코드 타임스탬프
    // static.field + static.value: 정적 값
}
```

### 3.4 기타 내장 Transform

| Transform | 기능 | 설정 예시 |
|-----------|------|----------|
| **Flatten** | 중첩 구조 평탄화 | `a.b.c` → `a_b_c` |
| **HoistField** | 값을 새 필드로 래핑 | `value` → `{"field": value}` |
| **ReplaceField** | 필드 이름 변경/제거 | `old_name` → `new_name` |
| **MaskField** | 필드 값 마스킹 | PII 보호 |
| **ExtractField** | Struct에서 단일 필드 추출 | `{"id": 1}` → `1` |
| **ValueToKey** | 값 필드를 키로 이동 | 키 기반 파티셔닝 |
| **RegexRouter** | 토픽 이름 정규식 변환 | `topic-(.*)` → `new-$1` |
| **TimestampRouter** | 타임스탬프 기반 토픽 라우팅 | `topic` → `topic-20240101` |
| **TimestampConverter** | 타임스탬프 형식 변환 | Unix ms → Date |
| **HeaderFrom** | 필드를 헤더로 이동 | 메타데이터 분리 |
| **InsertHeader** | 정적 헤더 삽입 | 라우팅 정보 추가 |
| **DropHeaders** | 헤더 제거 | 불필요 메타데이터 정리 |
| **SetSchemaMetadata** | 스키마 이름/버전 변경 | 스키마 레지스트리 호환 |

---

## 4. Predicate 시스템

### 4.1 조건부 변환

```
transforms = myTransform
transforms.myTransform.type = org.apache.kafka.connect.transforms.Filter
transforms.myTransform.predicate = hasHeader
transforms.myTransform.negate = true   # 조건 반전

predicates = hasHeader
predicates.hasHeader.type = org.apache.kafka.connect.transforms.predicates.HasHeaderKey
predicates.hasHeader.name = important
```

### 4.2 내장 Predicate

| Predicate | 조건 |
|-----------|------|
| HasHeaderKey | 특정 헤더 키 존재 여부 |
| RecordIsTombstone | 값이 null (tombstone) |
| TopicNameMatches | 토픽 이름 정규식 매칭 |

---

## 5. File Connector — 레퍼런스 구현

### 5.1 아키텍처

**소스 위치**: `connect/file/src/main/java/org/apache/kafka/connect/file/`

```
┌───────────────────────────────────────────────────┐
│              FileStreamSourceConnector             │
│                                                    │
│  ┌──────────────────────────────────────────────┐  │
│  │           FileStreamSourceTask               │  │
│  │                                              │  │
│  │  ┌──────┐   ┌────────────┐   ┌────────────┐ │  │
│  │  │ File │──▶│ Buffer     │──▶│SourceRecord│ │  │
│  │  │ (or  │   │ (char[]    │   │ (topic,    │ │  │
│  │  │ stdin)│   │  + offset) │   │  value,   │ │  │
│  │  └──────┘   └────────────┘   │  offset)  │ │  │
│  │                              └────────────┘ │  │
│  └──────────────────────────────────────────────┘  │
│                                                    │
│  ┌──────────────────────────────────────────────┐  │
│  │           FileStreamSinkTask                 │  │
│  │  SourceRecord → File (or stdout)             │  │
│  └──────────────────────────────────────────────┘  │
└───────────────────────────────────────────────────┘
```

### 5.2 FileStreamSourceConnector

```java
public class FileStreamSourceConnector extends SourceConnector {
    static final ConfigDef CONFIG_DEF = new ConfigDef()
        .define(FILE_CONFIG, Type.STRING, null, Importance.HIGH,
            "Source filename. If not specified, stdin will be used")
        .define(TOPIC_CONFIG, Type.STRING, ConfigDef.NO_DEFAULT_VALUE,
            new ConfigDef.NonEmptyString(), Importance.HIGH,
            "The topic to publish data to")
        .define(TASK_BATCH_SIZE_CONFIG, Type.INT, DEFAULT_TASK_BATCH_SIZE,
            Importance.LOW, "Max records per poll");

    @Override
    public List<Map<String, String>> taskConfigs(int maxTasks) {
        // 파일 소스는 항상 1개 태스크만 생성
        ArrayList<Map<String, String>> configs = new ArrayList<>();
        configs.add(props);
        return configs;
    }
}
```

**왜 1개 태스크만 생성하는가?**

단일 파일은 병렬로 읽을 수 없다. 여러 태스크가 동일 파일을 읽으면
중복 데이터가 발생한다.

### 5.3 FileStreamSourceTask — poll() 구현

```java
@Override
public List<SourceRecord> poll() throws InterruptedException {
    if (stream == null) {
        // 파일 열기 + 이전 오프셋 복구
        stream = Files.newInputStream(Paths.get(filename));
        Map<String, Object> offset = context.offsetStorageReader()
            .offset(Map.of(FILENAME_FIELD, filename));
        if (offset != null) {
            Long lastRecordedOffset = (Long) offset.get(POSITION_FIELD);
            if (lastRecordedOffset != null) {
                long skipLeft = lastRecordedOffset;
                while (skipLeft > 0) {
                    long skipped = stream.skip(skipLeft);
                    skipLeft -= skipped;
                }
            }
            streamOffset = lastRecordedOffset != null ? lastRecordedOffset : 0L;
        }
        reader = new BufferedReader(
            new InputStreamReader(stream, StandardCharsets.UTF_8));
    }

    // 비차단(non-blocking) 라인 읽기
    ArrayList<SourceRecord> records = null;
    while (reader.ready()) {
        nread = reader.read(buffer, offset, buffer.length - offset);
        if (nread > 0) {
            offset += nread;
            String line;
            do {
                line = extractLine();
                if (line != null) {
                    if (records == null) records = new ArrayList<>();
                    records.add(new SourceRecord(
                        offsetKey(filename),
                        offsetValue(streamOffset),
                        topic, null, null, null,
                        VALUE_SCHEMA, line,
                        System.currentTimeMillis()));
                    if (records.size() >= batchSize) return records;
                }
            } while (line != null);

            // 버퍼 확장 (라인이 버퍼보다 긴 경우)
            if (!foundOneLine && offset == buffer.length) {
                char[] newbuf = new char[buffer.length * 2];
                System.arraycopy(buffer, 0, newbuf, 0, buffer.length);
                buffer = newbuf;
            }
        }
    }
    return records;
}
```

### 5.4 extractLine — 라인 추출

```java
private String extractLine() {
    int until = -1, newStart = -1;
    for (int i = 0; i < offset; i++) {
        if (buffer[i] == '\n') {
            until = i;
            newStart = i + 1;
            break;
        } else if (buffer[i] == '\r') {
            if (i + 1 >= offset) return null;  // \r\n 확인 필요
            until = i;
            newStart = (buffer[i + 1] == '\n') ? i + 2 : i + 1;
            break;
        }
    }

    if (until != -1) {
        String result = new String(buffer, 0, until);
        System.arraycopy(buffer, newStart, buffer, 0, buffer.length - newStart);
        offset = offset - newStart;
        if (streamOffset != null)
            streamOffset += newStart;  // 바이트 오프셋 업데이트
        return result;
    }
    return null;
}
```

**왜 readLine()을 사용하지 않는가?**

`BufferedReader.readLine()`은 차단(blocking) 호출이다.
Connect 프레임워크는 `poll()`이 빠르게 반환되어야 하며,
데이터가 없으면 null을 반환하고 프레임워크가 백오프를 관리한다.
직접 `read()`와 버퍼 관리로 비차단 라인 읽기를 구현한다.

### 5.5 Exactly-Once 지원

```java
@Override
public ExactlyOnceSupport exactlyOnceSupport(Map<String, String> props) {
    String filename = parsedConfig.getString(FILE_CONFIG);
    return filename != null && !filename.isEmpty()
            ? ExactlyOnceSupport.SUPPORTED
            : ExactlyOnceSupport.UNSUPPORTED;
}
```

파일에서 읽을 때는 바이트 오프셋으로 정확한 위치를 복구할 수 있어 EOS 가능.
stdin에서 읽을 때는 오프셋 추적이 불가능하여 EOS 불가.

### 5.6 오프셋 관리

```java
// 오프셋 키: 파일 이름
private Map<String, String> offsetKey(String filename) {
    return Collections.singletonMap(FILENAME_FIELD, filename);
}

// 오프셋 값: 바이트 위치
private Map<String, Long> offsetValue(Long pos) {
    return Collections.singletonMap(POSITION_FIELD, pos);
}
```

---

## 6. SMT 개발 가이드

### 6.1 커스텀 SMT 구현 패턴

```java
public class MyTransform<R extends ConnectRecord<R>>
        implements Transformation<R> {

    @Override
    public void configure(Map<String, ?> configs) {
        // 설정 로드
    }

    @Override
    public R apply(R record) {
        // 1. operatingValue 추출
        // 2. 변환 로직
        // 3. newRecord 생성하여 반환
        return record.newRecord(
            record.topic(), record.kafkaPartition(),
            record.keySchema(), record.key(),
            newSchema, newValue,
            record.timestamp());
    }

    @Override
    public ConfigDef config() { return CONFIG_DEF; }
    @Override
    public void close() { }
}
```

### 6.2 성능 고려사항

| 항목 | 권장 사항 |
|------|----------|
| 스키마 캐싱 | SynchronizedCache + LRUCache 사용 |
| 불변 객체 | 변환된 레코드는 새 객체로 생성 |
| 예외 처리 | DataException으로 래핑 |
| null 처리 | null 레코드는 변환 없이 통과 |

---

## 7. 정리

### 핵심 설계 원칙

| 원칙 | 설명 |
|------|------|
| Key/Value 분리 | Template Method 패턴으로 키/값 독립 변환 |
| 스키마 인식 | Schema 유무에 따른 이중 경로 |
| LRU 캐싱 | 스키마 빌드 비용 최소화 |
| null = drop | 필터링을 위한 간결한 규약 |
| 바이트 오프셋 추적 | 파일 커넥터의 EOS 기반 |
| 비차단 읽기 | Connect poll() 계약 준수 |

---

*소스 참조: connect/transforms/src/.../transforms/, connect/file/src/.../file/*
