# Mermaid.js 치트시트

GitHub에서 이 파일을 열면 다이어그램이 자동 렌더링됩니다.

## 1. 플로우차트 노드 모양

```mermaid
graph LR
    A[사각형] --> B(둥근 사각형)
    B --> C([스타디움])
    C --> D[[서브루틴]]
    D --> E[(데이터베이스)]
    E --> F((원))
    F --> G{다이아몬드}
    G --> H[/평행사변형/]
    H --> I[\역평행사변형\]
    I --> J[/사다리꼴\]
```

## 2. 플로우차트 화살표 종류

```mermaid
graph LR
    A -->|실선 화살표| B
    C ---|실선| D
    E -.->|점선 화살표| F
    G ==>|굵은 화살표| H
    I --o|원 끝| J
    K --x|X 끝| L
```

## 3. 플로우차트 방향

```mermaid
graph TD
    A[TD: Top to Down]
    A --> B
```

```mermaid
graph LR
    A[LR: Left to Right]
    A --> B
```

## 4. 시퀀스 다이어그램 메시지 종류

```mermaid
sequenceDiagram
    A->>B: 동기 요청 (실선, 채운 화살표)
    B-->>A: 동기 응답 (점선, 채운 화살표)
    A-)B: 비동기 메시지 (실선, 빈 화살표)
    B--)A: 비동기 응답 (점선, 빈 화살표)
```

## 5. 시퀀스 다이어그램 제어 구조

```mermaid
sequenceDiagram
    participant A
    participant B

    alt 조건 A
        A->>B: A 처리
    else 조건 B
        A->>B: B 처리
    end

    opt 선택적 처리
        A->>B: 로깅
    end

    loop 매 초마다
        A->>B: 하트비트
    end

    par 병렬 처리
        A->>B: 작업 1
    and
        A->>B: 작업 2
    end
```

## 6. 클래스 다이어그램 관계

```mermaid
classDiagram
    classA --|> classB : 상속 (Inheritance)
    classC --* classD : 합성 (Composition)
    classE --o classF : 집합 (Aggregation)
    classG --> classH : 연관 (Association)
    classI ..> classJ : 의존 (Dependency)
    classK ..|> classL : 구현 (Implementation)
```

## 7. 상태 다이어그램

```mermaid
stateDiagram-v2
    [*] --> Idle
    Idle --> Processing : start
    Processing --> Success : done
    Processing --> Error : fail
    Error --> Idle : retry
    Success --> [*]
```

## 8. ERD 관계 기호

```mermaid
erDiagram
    A ||--|| B : "1:1 (정확히 하나)"
    C ||--o{ D : "1:N (0개 이상)"
    E ||--|{ F : "1:N (1개 이상)"
    G }o--o{ H : "N:M (0개 이상)"
```

## 9. 스타일링

```mermaid
graph LR
    A[기본] --> B[빨강]
    A --> C[파랑]
    A --> D[초록]

    style B fill:#ff6b6b,color:white,stroke:#c0392b
    style C fill:#74b9ff,color:white,stroke:#2980b9
    style D fill:#55efc4,color:black,stroke:#00b894
```
