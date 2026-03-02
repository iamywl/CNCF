# 12. Markdown 작성 가이드 — README의 기술

## 개념

Markdown은 **텍스트 기반 마크업 언어**로, 간단한 문법으로 서식이 있는 문서를 작성합니다.
GitHub, GitLab, Notion, Slack 등 개발 생태계 전반에서 사용되는 **사실상의 표준 문서 형식**입니다.

## 왜 필요한가?

- **README.md**는 프로젝트의 첫인상 — 잘 작성된 README가 기여자를 끌어들임
- Git에서 **diff/merge 가능** — 바이너리 문서(Word, PDF)와 달리 버전 관리 용이
- GitHub이 자동 렌더링 — 별도 도구 없이 웹에서 바로 확인

## Markdown 핵심 문법

### 기본 서식
```markdown
**볼드**, *이탤릭*, ~~취소선~~, `인라인 코드`
```

### 헤딩
```markdown
# H1 (프로젝트명)
## H2 (대분류)
### H3 (소분류)
```

### 코드 블록
````markdown
```javascript
const hello = "world";
```
````

### 테이블
```markdown
| 이름 | 역할 | 담당 |
|------|------|------|
| 홍길동 | Backend | API 개발 |
| 김철수 | Frontend | UI 개발 |
```

### 링크/이미지
```markdown
[링크 텍스트](https://example.com)
![이미지 대체텍스트](./images/screenshot.png)
```

### 체크리스트
```markdown
- [x] 완료된 항목
- [ ] 미완료 항목
```

### 접기(Details)
```markdown
<details>
<summary>클릭하여 펼치기</summary>

숨겨진 내용입니다.

</details>
```

## POC 프로젝트

README 템플릿과 다양한 Markdown 문법 예시를 제공합니다.

### 실행 방법

```bash
cd 12-markdown-guide

# GitHub에서 보거나, VSCode Preview(Cmd+Shift+V)로 확인
# 또는 grip으로 로컬 프리뷰 (pip install grip)
grip cheatsheet.md
```

### 파일 구조

```
12-markdown-guide/
├── README.md              ← 지금 보고 있는 파일
├── cheatsheet.md          ← Markdown 전체 문법 치트시트
├── templates/
│   ├── project-readme.md  ← 프로젝트 README 템플릿
│   ├── api-readme.md      ← API 프로젝트 README 템플릿
│   └── contributing.md    ← CONTRIBUTING.md 템플릿
└── examples/
    └── good-readme.md     ← 잘 작성된 README 예시
```

## 학습 포인트

1. **README는 프로젝트의 얼굴**: 설치, 실행, 기여 방법을 반드시 포함
2. **템플릿 활용**: 매번 처음부터 쓰지 말고 템플릿에서 시작
3. **뱃지(Badge) 활용**: CI 상태, 버전, 라이선스를 시각적으로 표시
4. **GFM(GitHub Flavored Markdown)**: 테이블, 체크리스트, 자동 링크 등 확장 문법
