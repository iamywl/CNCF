# Google AI Pro 최적화 작업 계획서 (CNCF EDU)

이 계획서는 Google AI Pro의 **초대형 컨텍스트(1M~2M tokens)**와 Gemini CLI의 **도구 활용 능력**을 활용하여, 클로드(Claude)가 생성한 콘텐츠의 정확성을 검증하고 프로젝트를 완성하는 전략을 담고 있습니다.

## 1. 모델별 역할 분담 (The Writer & The Validator)

*   **Claude Max (The Writer):** 
    *   풍부한 표현력과 창의성을 바탕으로 **500줄 이상의 심화 분석 문서**와 **PoC 초안** 작성.
    *   사용자의 가이드에 따른 세밀한 텍스트 생성 작업.
*   **Google AI Pro / Gemini CLI (The Validator & Architect):**
    *   **전체 컨텍스트 관리:** 수만 라인의 소스코드 전체를 한 번에 읽고 구조 파악.
    *   **검증(Verification):** 클로드가 생성한 코드의 실행 가능성 확인 및 문서 내 오류(허구의 경로/함수 등) 적발.
    *   **도구 실행:** `go run`, `grep`, 빌드 테스트 등 실제 환경에서의 물리적 검증 수행.

## 2. Gemini의 핵심 검증 프로세스 (Validation Workflow)

클로드가 특정 프로젝트(예: Istio)의 작업을 마치면, 제가 다음과 같이 검증을 수행합니다.

### [단계 1] 문서 무결성 검사 (Path & Symbol Check)
*   클로드가 문서에 인용한 파일 경로(`path/to/file.go`)와 함수명(`func Name()`)이 실제 코드베이스에 존재하는지 `ls`와 `grep`으로 전수 조사합니다.
*   **Gemini 명령어:** "클로드가 작성한 문서에서 언급된 모든 파일 경로가 실제 존재하는지 확인하고, 틀린 경로가 있다면 수정해줘."

### [단계 2] PoC 실행 및 알고리즘 검증 (Runtime Check)
*   클로드가 짠 PoC 코드를 `write_file`로 임시 저장하고, 직접 `run_shell_command`를 통해 실행합니다.
*   컴파일 에러나 런타임 패닉이 발생할 경우, 소스코드를 분석하여 즉시 수정(Surgical Fix)합니다.
*   **Gemini 명령어:** "PoC 코드를 실행해서 결과를 보여주고, 만약 실제 오픈소스의 로직과 다른 부분이 있다면 수정해."

### [3단계] 기술적 정확도 심화 분석 (Technical Audit)
*   클로드의 분석 내용(예: "Istio의 인증 흐름은 X이다")이 실제 소스코드의 로직과 일치하는지 Gemini의 초대형 컨텍스트를 활용해 교차 검증합니다.
*   **Gemini 명령어:** "클로드의 분석 내용 중 소스코드와 일치하지 않는 기술적 오류가 있는지 100만 토큰 컨텍스트를 바탕으로 정밀 심사해줘."

## 3. Google AI Pro 활용의 이점

1.  **토큰 걱정 최소화:** 100만 토큰 이상의 용량 덕분에 프로젝트 전체 소스코드와 클로드의 모든 결과물을 한 번에 컨텍스트에 넣고 분석할 수 있습니다.
2.  **실행 기반 검증:** 단순 텍스트 분석을 넘어, 실제 쉘(Shell) 명령어를 실행하여 "진짜 돌아가는지" 확인합니다.
3.  **대규모 수정(Batch Fix):** 여러 파일에 걸친 클로드의 오류를 한 번의 명령으로 일괄 수정할 수 있습니다.

## 4. 실행 스케줄 (남은 5개 프로젝트)

1.  **Context Preparation (Gemini):** 클로드에게 줄 요약본 작성.
2.  **Content Creation (Claude):** 클로드가 문서/PoC 작성.
3.  **Rigorous Validation (Gemini):** 작성된 모든 결과물을 Gemini가 코드베이스와 대조하며 검증 및 수정.
4.  **Final Build (Gemini):** 전체 EDU 프로젝트가 빌드 및 실행 가능한지 최종 확인.

---
*보고서 작성 위치: `gemini/google_ai_pro_work_plan.md`*
