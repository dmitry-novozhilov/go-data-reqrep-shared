# go-data-reqrep-shared

Go-реализация высокопроизводительного request/reply IPC через разделяемую память (mmap) для Linux.

Побайтово совместима с [`perl5-data-reqrep-shared`](https://github.com/vividsnow/perl5-data-reqrep-shared) (C/XS): Go-сервер и Perl-клиент работают через один mmap-файл, и наоборот.

## Особенности

- **Транспорт:** mmap-файл (`MAP_SHARED`) — нет сокетов, нет pipe
- **Синхронизация:** spin (32 итерации, `PAUSE`) → Linux futex; без CGo
- **Конкурентность:** MPMC — несколько клиентов и несколько серверов одновременно
- **Совместимость:** string mode (`mode=0`), полный binary layout из `reqrep.h`
- **Отказоустойчивость:** stale recovery по PID, cancel с generation-защитой от ABA
- **Только Linux**, только одна машина

## Установка

```
go get git.adtech.love/shared-libs/go-data-reqrep-shared
```

## Использование

```go
import reqrep "git.adtech.love/shared-libs/go-data-reqrep-shared"

// Сервер
srv, _ := reqrep.NewServer("/tmp/myservice.reqrep", reqrep.Options{})
defer srv.Close()
srv.Serve(ctx, func(req []byte) ([]byte, error) {
    return process(req), nil
})

// Клиент
cli, _ := reqrep.NewClient("/tmp/myservice.reqrep")
defer cli.Close()
resp, err := cli.Query(ctx, []byte("hello"))
```

`NewServer` создаёт файл при первом запуске; повторный вызов открывает существующий. `NewClient` открывает файл, созданный любой стороной — Go или Perl.

## Настройки

```go
type Options struct {
    ReqCap      uint32 // ёмкость очереди запросов (степень 2), default 256
    ReqArenaCap uint32 // буфер данных запросов, default ReqCap×256
    RespSlots   uint32 // слоты ответов, default ReqCap
    RespDataMax uint32 // макс. байт ответа, default 4096
    SpinCount   int    // spin перед futex, default 32
}
```

Параметры фиксируются при создании файла — `NewServer` на существующем файле игнорирует `Options`.

## Make-цели

| Цель | Действие |
|---|---|
| `make test` | `go test -v -race ./...` |
| `make bench` | бенчмарки латентности и пропускной способности |
| `make generate` | перегенерировать `reqrep_layout_gen.go` из `gen/reqrep.h` |
| `make update-header` | скачать свежий `reqrep.h` и перегенерировать |

## Структура

```
reqrep.go               — Server, Client, публичный API
reqrep_queue.go         — enqueue/dequeue (Vyukov MPMC через mutex)
reqrep_slots.go         — acquire/release resp-слота, stale recovery
reqrep_arena.go         — circular arena для данных запросов
reqrep_mutex.go         — arena mutex (spin → futex)
reqrep_futex_linux.go   — futexWait/futexWake, spinPause
reqrep_layout.go        — accessor-обёртки над полями shared memory
reqrep_layout_gen.go    — GENERATED: смещения и константы из reqrep.h
gen/                    — кодогенератор (gcc + C-программа с offsetof)
```

## Совместимость с Perl

Один и тот же `/tmp/myservice.reqrep` открывается Go и Perl без каких-либо изменений. Формат файла определяется `reqrep.h`; layout Go-кода генерируется из него автоматически через `make generate`.


# AI usage stats
<!--AI-usage-->
| :robot: model | requests | input tokens | output tokens | cache read input tokens | cache creation input tokens |
| :--- | ---: | ---: | ---: | ---: | ---: |
| claude-haiku-4-5-20251001 | 2.0  | 356.0  | 396.0  | 16.7 K | 18.1 K |
| claude-opus-4-7 | 99.0  | 189.0  | 17.3 K | 3.6 M | 564.5 K |
| claude-sonnet-4-6 | 391.0  | 535.0  | 384.2 K | 39.7 M | 1.8 M |
<!--AI-usage-->
