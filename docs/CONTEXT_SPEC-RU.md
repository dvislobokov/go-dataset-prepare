# Контекст проекта и каретки в промпте: `flc-context/v1`

Спецификация общая для C# и Go и ведётся в одном месте, чтобы не расходиться:
https://github.com/dvislobokov/csharp-dataset-prepare/blob/main/docs/CONTEXT_SPEC-RU.md

Для Go в ней отдельно оговорены: профиль `DEPS` по путям импорта (раздел 4), отсутствие строки `PROPERTY`, получатель метода
в `ARG`, `RECV package X` после `пакет.` (раздел 5.1). Факты в наших данных — конфигурация `semantic` датасета
`dvislobokov/go-ml-complation` (`go/types` на снимке без недописанной части строки).
