# Техническое задание: Go-реализация shared-memory req/rep IPC

## Контекст и цель

Реализовать высокопроизводительный request/response IPC между процессами на одной Linux-машине через разделяемую память (mmap). Реализация должна быть **побайтово совместима** с библиотекой [`perl5-data-reqrep-shared`](https://github.com/vividsnow/perl5-data-reqrep-shared) (C/XS): Go-процесс (клиент или сервер) должен работать в паре с Perl-процессом, использующим эту библиотеку.

**Целевой trade-off:** минимальная латентность при низком idle-CPU.  
Механизм: spinlock (32 итерации, `PAUSE`/`YIELD`) → futex wait.

**Только Linux**, только одна машина.

---

## Совместимость с Perl-реализацией

Требования к совместимости:

1. Одинаковый файл mmap — Go и Perl открывают один и тот же `/tmp/<name>.reqrep` (или любой путь).
2. Go может быть **клиентом** при Perl-сервере и **сервером** при Perl-клиенте.
3. Файл может быть создан любой из сторон — другая открывает существующий.
4. Используется **string mode** (`mode = REQREP_MODE_STR = 0`) — Perl работает только в нём.
5. Все поля в shared memory — `uint32_t` / `uint64_t` в native byte order (little-endian на x86/ARM).
6. Go **не использует Go-структуры** для доступа к shared memory — только `unsafe.Pointer` + арифметика смещений, совпадающих с C-лэйаутом.
7. Атомарные операции Go (`sync/atomic`) на x86-64 и ARM64 совместимы на hardware-уровне с `__atomic_*` GCC с теми же ordering guarantees.

---

## Точный memory layout (из reqrep.h)

### Магия и константы

```
magic:         0x52525331  ("RRS1")
version:       1
mode str:      0  (REQREP_MODE_STR)
mode int:      1  (REQREP_MODE_INT) — не использовать при Perl-совместимости

RESP_FREE:     0
RESP_ACQUIRED: 1
RESP_READY:    2

REQREP_UTF8_FLAG:    0x80000000
REQREP_STR_LEN_MASK: 0x7FFFFFFF

MUTEX_WRITER_BIT: 0x80000000
MUTEX_PID_MASK:   0x7FFFFFFF
# mutex locked = MUTEX_WRITER_BIT | (pid & MUTEX_PID_MASK)

spin_limit:    32
lock_timeout:  2 секунды
```

### ReqRepHeader (256 байт, 4 cache line по 64 байта)

```
Cache line 0 — неизменяемая конфигурация (offs 0..63):
  0   magic          uint32   // 0x52525331
  4   version        uint32   // 1
  8   mode           uint32   // 0 = str, 1 = int
  12  req_cap        uint32   // ёмкость очереди запросов (степень 2)
  16  total_size     uint64   // полный размер mmap-региона
  24  req_slots_off  uint32   // offset массива ReqSlot от начала mmap
  28  req_arena_off  uint32   // offset arena от начала mmap
  32  req_arena_cap  uint32   // размер arena в байтах
  36  resp_slots     uint32   // число response slot'ов
  40  resp_data_max  uint32   // макс. размер данных ответа (байт)
  44  resp_off       uint32   // offset массива RespSlotHeader от начала mmap
  48  resp_stride    uint32   // шаг между слотами (32 + resp_data_max, выровн. по 64)
  52  _pad0[12]      uint8[12]

Cache line 1 — очередь запросов, потребление (offs 64..127):
  64  req_head       uint64   // Vyukov head (монотонный)
  72  recv_waiters   uint32   // число серверов, ждущих запрос
  76  recv_futex     uint32   // futex word: сигнал серверу
  80  _pad1[48]      uint8[48]

Cache line 2 — очередь запросов, производство (offs 128..191):
  128 req_tail       uint64   // Vyukov tail (монотонный)
  136 send_waiters   uint32   // число клиентов, ждущих место в очереди
  140 send_futex     uint32   // futex word: сигнал клиенту при переполнении
  144 _pad2[48]      uint8[48]

Cache line 3 — mutex, arena, статистика (offs 192..255):
  192 mutex          uint32   // arena mutex (PID | WRITER_BIT, или 0)
  196 mutex_waiters  uint32
  200 arena_wpos     uint32   // позиция записи в arena (байты)
  204 arena_used     uint32   // занято байт в arena
  208 resp_hint      uint32   // подсказка для сканирования свободных resp slot'ов
  212 stat_recoveries uint32
  216 slot_futex     uint32   // futex word: освобождение resp slot'а
  220 slot_waiters   uint32
  224 stat_requests  uint64
  232 stat_replies   uint64
  240 stat_send_full uint64
  248 stat_recv_empty uint32
  252 _pad3          uint32
```

### ReqSlot (24 байта, string mode)

```
  0  arena_off   uint32  // offset данных запроса в arena
  4  packed_len  uint32  // длина + UTF8-флаг (len & STR_LEN_MASK)
  8  arena_skip  uint32  // байты, пропущенные при wraparound в arena
  12 resp_slot   uint32  // индекс response slot
  16 resp_gen    uint32  // generation response slot (ABA protection)
  20 _rpad       uint32
```

Массив ReqSlot начинается с `req_slots_off`. Индекс слота = `pos % req_cap`.  
Для Vyukov MPMC в string mode **sequence number хранится не в ReqSlot**, а вычисляется через `req_head` / `req_tail` и `req_cap`. (Отличие от int mode, где `sequence` — первое поле ReqIntSlot.)

### RespSlotHeader (32 байта) + данные ответа

```
  0  state       uint32  // futex word: RESP_FREE=0, RESP_ACQUIRED=1, RESP_READY=2
  4  waiters     uint32  // число клиентов, ждущих именно этот слот
  8  owner_pid   uint32  // PID владельца (для stale recovery)
  12 resp_len    uint32  // длина ответа
  16 resp_flags  uint32  // флаги (REQREP_UTF8_FLAG)
  20 generation  uint32  // монотонно растёт при каждом переиспользовании
  24 _rpad[2]    uint32[2]
```

Данные ответа: `resp_stride - 32` байт сразу после заголовка.  
`resp_stride` = выровненный по 64 байта `(32 + resp_data_max)`.  
Offset слота i: `resp_off + i * resp_stride`.

### Request Arena (circular buffer)

Сырой буфер байт, размером `req_arena_cap`.  
`arena_wpos` — позиция следующей записи (wraps around).  
`arena_skip` в ReqSlot — сколько байт было пропущено в конце при wraparound.  
Чтение: `data = arena[arena_off : arena_off + (packed_len & STR_LEN_MASK)]`.

### Request ID

```
id = (uint64(generation) << 32) | uint64(slot_index)
slot_index = uint32(id & 0xFFFFFFFF)
generation = uint32(id >> 32)
```

---

## Протокол (string mode)

### Клиент — отправка запроса

```
1. AcquireRespSlot():
   - Начать с resp_hint как стартового индекса сканирования
   - CAS(slot.state, RESP_FREE → RESP_ACQUIRED) по каждому слоту
   - При успехе: записать owner_pid = getpid(), инкрементировать generation
   - При отсутствии свободных:
       incr(slot_waiters)
       futex_wait(&slot_futex, current_val)
       decr(slot_waiters)
       повторить
   - Вернуть id = (generation << 32) | slotIndex

2. WriteArena(data []byte):
   - mutex_lock(&mutex, &mutex_waiters)  // spin(32) → futex
   - Проверить, помещается ли data с текущего arena_wpos до конца
   - Если нет: arena_skip = req_arena_cap - arena_wpos; arena_wpos = 0
   - Скопировать data в arena[arena_wpos:]
   - arena_off = arena_wpos; arena_wpos += len(data); arena_used += len(data)
   - mutex_unlock(&mutex, &mutex_waiters)

3. WriteReqSlot(pos uint64, respSlotIdx, respGen uint32, arenaOff, dataLen, arenaSkip uint32):
   - slot = &reqSlots[pos % req_cap]
   - Дождаться, пока слот освободится (spin на seq в int mode; в str mode — иначе, см. ниже)
   - Заполнить поля slot
   - Опубликовать: fence (release)

   Примечание о Vyukov в string mode:
   Perl-реализация защищает очередь запросов mutex'ом, а не Vyukov-sequence в ReqSlot.
   Необходимо сверить точный алгоритм enqueue в reqrep.h — mutex или lockfree.

4. Signal:
   - atomic.AddUint32(&req_tail, 1) или иная публикация (точный порядок из reqrep.h)
   - Если recv_waiters > 0:
       atomic.AddUint32(&recv_futex, 1)
       futex_wake(&recv_futex, 1)

5. WaitResponse(ctx, id):
   - spin(32): if slot.state == RESP_READY && slot.generation == gen → done
   - incr(slot.waiters)
   - futex_wait(&slot.state, RESP_ACQUIRED, deadline_from_ctx)
   - decr(slot.waiters)
   - повторить или return ctx.Err()

6. ReadResponse():
   - data = respArea[slotIdx*resp_stride+32 : ... +32+resp_len]
   - result = copy(data)
   - CAS(slot.state, RESP_READY → RESP_FREE)
   - incr(slot.generation)
   - Если slot_waiters > 0: futex_wake(&slot_futex, 1)
```

### Сервер — обработка запроса

```
1. DequeueRequest():
   - Если req_head == req_tail (пусто):
       incr(recv_waiters)
       futex_wait(&recv_futex, current_val, timeout=2s)  // periodic stale check
       decr(recv_waiters)
       повторить
   - pos = atomic.AddUint64(&req_head, 1) - 1
   - slot = &reqSlots[pos % req_cap]
   - Прочитать arena_off, packed_len, arena_skip, resp_slot, resp_gen
   - data = arena[arena_off : arena_off + (packed_len & STR_LEN_MASK)]
   - Освободить arena: atomic.AddUint32(&arena_used, -(arena_skip + dataLen))

2. handler(data []byte) ([]byte, error)

3. WriteResponse(respSlotIdx, respGen uint32, response []byte):
   - slot = &respSlots[respSlotIdx]
   - Проверить: slot.generation == respGen (клиент мог отменить)
   - Скопировать response в respArea[respSlotIdx*resp_stride+32:]
   - slot.resp_len = len(response)
   - CAS(slot.state, RESP_ACQUIRED → RESP_READY)  // release
   - Если slot.waiters > 0: futex_wake(&slot.state, 1)
   - incr(stat_replies)
```

### CancelRequest(id)

```
slotIdx = uint32(id)
gen     = uint32(id >> 32)
slot    = &respSlots[slotIdx]
if slot.generation != gen: return  // уже переиспользован
CAS(slot.state, RESP_ACQUIRED → RESP_FREE)
если успех:
    incr(slot.generation)
    если slot_waiters > 0: futex_wake(&slot_futex, 1)
```

### Stale Recovery

```
// В AcquireRespSlot при сканировании:
if slot.state == RESP_ACQUIRED && !pidAlive(slot.owner_pid):
    CAS(slot.state, RESP_ACQUIRED → RESP_FREE)
    incr(slot.generation)
    incr(stat_recoveries)

// В сервере при таймауте futex_wait (2с):
if mutex != 0 && !pidAlive(mutex & MUTEX_PID_MASK):
    CAS(&mutex, locked_val, 0)
    futex_wake(&mutex, 1)

func pidAlive(pid uint32) bool:
    err := syscall.Kill(int(pid), 0)
    return err == nil || err == syscall.EPERM
```

---

## Важное замечание о Vyukov в string mode

В **int mode** Perl-реализация использует классический Vyukov MPMC: поле `sequence` в `ReqIntSlot` — это sequence number слота.  
В **string mode** очередь запросов защищена **mutex'ом** (`header.mutex`), а не sequence числами. Это необходимо из-за переменной длины данных в arena.

**Требование:** перед реализацией сверить точный алгоритм enqueue/dequeue string mode в `reqrep.h` — использует ли он mutex или иной механизм для сериализации записи в ReqSlot. Это критично для совместимости.

---

## Go API

```go
package reqrep

type Server struct { ... }

// NewServer создаёт или открывает файл mmap и инициализирует header.
// Если файл уже существует с корректным magic/version/mode — открывает как есть.
func NewServer(path string, opts Options) (*Server, error)

// Serve блокируется, вызывая handler для каждого запроса.
// Несколько горутин могут вызывать Serve одновременно (MPMC).
func (s *Server) Serve(ctx context.Context, handler func(req []byte) ([]byte, error))

func (s *Server) Close() error

type Client struct { ... }

// NewClient открывает существующий файл mmap.
// Файл должен быть создан сервером (Go или Perl) заранее.
func NewClient(path string) (*Client, error)

// Query потокобезопасен — несколько горутин могут вызывать одновременно.
func (c *Client) Query(ctx context.Context, req []byte) ([]byte, error)

func (c *Client) Close() error

type Options struct {
    ReqCap      uint32 // степень 2, default 256; игнорируется при открытии существующего
    ReqArenaCap uint32 // default ReqCap * RespDataMax; игнорируется при открытии существующего
    RespSlots   uint32 // default ReqCap; игнорируется при открытии существующего
    RespDataMax uint32 // default 4096; игнорируется при открытии существующего
    SpinCount   int    // default 32
}
```

---

## Go-специфика реализации

### Доступ к shared memory

Все поля header, слотов и arena читаются/пишутся через `unsafe.Pointer` + арифметику смещений. **Никаких Go-структур, отображённых на shared memory** — выравнивание и padding у Go и C могут различаться.

```go
// Пример:
func hdrReqTail(base unsafe.Pointer) *uint64 {
    return (*uint64)(unsafe.Pointer(uintptr(base) + 128))
}
func hdrRecvFutex(base unsafe.Pointer) *uint32 {
    return (*uint32)(unsafe.Pointer(uintptr(base) + 76))
}
```

Все atomic-операции — через `sync/atomic` (`atomic.LoadUint32`, `atomic.CompareAndSwapUint32`, и т.д.).

### Futex

```go
// reqrep_futex_linux.go
import "golang.org/x/sys/unix"

func futexWait(addr *uint32, val uint32, timeout *unix.Timespec) error {
    _, _, errno := unix.Syscall6(unix.SYS_FUTEX,
        uintptr(unsafe.Pointer(addr)),
        uintptr(unix.FUTEX_WAIT|unix.FUTEX_PRIVATE_FLAG),
        uintptr(val),
        uintptr(unsafe.Pointer(timeout)),
        0, 0)
    if errno != 0 && errno != syscall.EAGAIN && errno != syscall.EINTR {
        return errno
    }
    return nil
}

func futexWake(addr *uint32, n uint32) {
    unix.Syscall6(unix.SYS_FUTEX,
        uintptr(unsafe.Pointer(addr)),
        uintptr(unix.FUTEX_WAKE|unix.FUTEX_PRIVATE_FLAG),
        uintptr(n),
        0, 0, 0)
}
```

**Важно:** `FUTEX_PRIVATE_FLAG` можно использовать только если все процессы используют его. Perl-реализация может использовать обычный `FUTEX_WAIT` (без PRIVATE). Нужно сверить `reqrep.h` — если там нет `FUTEX_PRIVATE_FLAG`, то Go тоже не должен его использовать.

### Mutex (arena)

Реализовать mutex идентично Perl:
```
lock:   spin(32) { CAS(&mutex, 0, WRITER_BIT | (pid & PID_MASK)) }
        → если не получилось: incr(mutex_waiters), futex_wait(&mutex, locked_val), decr(mutex_waiters), повторить
unlock: store(&mutex, 0)
        → если mutex_waiters > 0: futex_wake(&mutex, 1)
```

### mmap

```go
data, err := unix.Mmap(fd, 0, int(totalSize),
    unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
```

---

## Кодогенерация из reqrep.h

### Проблема

Смещения полей (`offsetof`) и константы (`#define`) в `reqrep.h` могут меняться при развитии C-библиотеки. Если Go-код хардкодит смещения вручную — рассинхронизация гарантирована. Нужна автоматическая генерация `reqrep_layout_gen.go` из исходного `reqrep.h`.

### Архитектура генератора

```
gen/
  reqrep.h          — вендорная копия C-заголовка (обновляется вручную или скриптом)
  gen_layout.c      — C-программа, печатающая offsetof/sizeof/константы
  gen.go            — //go:build ignore; запускает gcc, парсит вывод, пишет Go-файл
```

`reqrep_layout_gen.go` — генерируемый файл с пометкой `// Code generated by gen/gen.go. DO NOT EDIT.`

### gen_layout.c

```c
#include "reqrep.h"
#include <stddef.h>
#include <stdio.h>

int main(void) {
    // Магия и версия
    printf("Magic        = 0x%08X\n", REQREP_MAGIC);
    printf("Version      = %u\n",     REQREP_VERSION);

    // Константы состояний и флагов
    printf("RespFree     = %d\n", RESP_FREE);
    printf("RespAcquired = %d\n", RESP_ACQUIRED);
    printf("RespReady    = %d\n", RESP_READY);
    printf("ModeStr      = %d\n", REQREP_MODE_STR);
    printf("Utf8Flag     = 0x%08X\n", REQREP_UTF8_FLAG);
    printf("StrLenMask   = 0x%08X\n", REQREP_STR_LEN_MASK);
    printf("MutexWriterBit = 0x%08X\n", REQREP_MUTEX_WRITER_BIT);
    printf("MutexPidMask   = 0x%08X\n", REQREP_MUTEX_PID_MASK);
    printf("SpinLimit    = %d\n", REQREP_SPIN_LIMIT);

    // Размеры структур
    printf("SizeofHeader    = %zu\n", sizeof(ReqRepHeader));
    printf("SizeofReqSlot   = %zu\n", sizeof(ReqSlot));
    printf("SizeofRespSlot  = %zu\n", sizeof(RespSlotHeader));

    // Смещения полей ReqRepHeader
    printf("HdrMagic         = %zu\n", offsetof(ReqRepHeader, magic));
    printf("HdrVersion       = %zu\n", offsetof(ReqRepHeader, version));
    printf("HdrMode          = %zu\n", offsetof(ReqRepHeader, mode));
    printf("HdrReqCap        = %zu\n", offsetof(ReqRepHeader, req_cap));
    printf("HdrTotalSize     = %zu\n", offsetof(ReqRepHeader, total_size));
    printf("HdrReqSlotsOff   = %zu\n", offsetof(ReqRepHeader, req_slots_off));
    printf("HdrReqArenaOff   = %zu\n", offsetof(ReqRepHeader, req_arena_off));
    printf("HdrReqArenaCap   = %zu\n", offsetof(ReqRepHeader, req_arena_cap));
    printf("HdrRespSlots     = %zu\n", offsetof(ReqRepHeader, resp_slots));
    printf("HdrRespDataMax   = %zu\n", offsetof(ReqRepHeader, resp_data_max));
    printf("HdrRespOff       = %zu\n", offsetof(ReqRepHeader, resp_off));
    printf("HdrRespStride    = %zu\n", offsetof(ReqRepHeader, resp_stride));
    printf("HdrReqHead       = %zu\n", offsetof(ReqRepHeader, req_head));
    printf("HdrRecvWaiters   = %zu\n", offsetof(ReqRepHeader, recv_waiters));
    printf("HdrRecvFutex     = %zu\n", offsetof(ReqRepHeader, recv_futex));
    printf("HdrReqTail       = %zu\n", offsetof(ReqRepHeader, req_tail));
    printf("HdrSendWaiters   = %zu\n", offsetof(ReqRepHeader, send_waiters));
    printf("HdrSendFutex     = %zu\n", offsetof(ReqRepHeader, send_futex));
    printf("HdrMutex         = %zu\n", offsetof(ReqRepHeader, mutex));
    printf("HdrMutexWaiters  = %zu\n", offsetof(ReqRepHeader, mutex_waiters));
    printf("HdrArenaWpos     = %zu\n", offsetof(ReqRepHeader, arena_wpos));
    printf("HdrArenaUsed     = %zu\n", offsetof(ReqRepHeader, arena_used));
    printf("HdrRespHint      = %zu\n", offsetof(ReqRepHeader, resp_hint));
    printf("HdrStatRecoveries= %zu\n", offsetof(ReqRepHeader, stat_recoveries));
    printf("HdrSlotFutex     = %zu\n", offsetof(ReqRepHeader, slot_futex));
    printf("HdrSlotWaiters   = %zu\n", offsetof(ReqRepHeader, slot_waiters));
    printf("HdrStatRequests  = %zu\n", offsetof(ReqRepHeader, stat_requests));
    printf("HdrStatReplies   = %zu\n", offsetof(ReqRepHeader, stat_replies));

    // Смещения полей ReqSlot
    printf("ReqArenaOff  = %zu\n", offsetof(ReqSlot, arena_off));
    printf("ReqPackedLen = %zu\n", offsetof(ReqSlot, packed_len));
    printf("ReqArenaSkip = %zu\n", offsetof(ReqSlot, arena_skip));
    printf("ReqRespSlot  = %zu\n", offsetof(ReqSlot, resp_slot));
    printf("ReqRespGen   = %zu\n", offsetof(ReqSlot, resp_gen));

    // Смещения полей RespSlotHeader
    printf("RespState      = %zu\n", offsetof(RespSlotHeader, state));
    printf("RespWaiters    = %zu\n", offsetof(RespSlotHeader, waiters));
    printf("RespOwnerPid   = %zu\n", offsetof(RespSlotHeader, owner_pid));
    printf("RespLen        = %zu\n", offsetof(RespSlotHeader, resp_len));
    printf("RespFlags      = %zu\n", offsetof(RespSlotHeader, resp_flags));
    printf("RespGeneration = %zu\n", offsetof(RespSlotHeader, generation));
    printf("RespDataStart  = %zu\n", sizeof(RespSlotHeader)); // данные после заголовка

    return 0;
}
```

### gen.go (//go:build ignore)

```go
//go:build ignore

package main

// Компилирует gen_layout.c, запускает бинарь, парсит вывод key=value,
// генерирует reqrep_layout_gen.go с Go-константами.
// Запуск: go generate ./...

import (
    "bytes"
    "fmt"
    "os"
    "os/exec"
    "path/filepath"
    "strings"
    "text/template"
    "time"
)

func main() {
    dir := filepath.Dir(os.Args[0])    // директория gen/
    outDir := filepath.Join(dir, "..") // директория пакета reqrep

    // Компиляция
    bin := filepath.Join(os.TempDir(), "reqrep_gen_layout")
    cmd := exec.Command("gcc", "-o", bin,
        filepath.Join(dir, "gen_layout.c"),
        "-I", dir,
    )
    cmd.Stderr = os.Stderr
    if err := cmd.Run(); err != nil {
        fmt.Fprintln(os.Stderr, "gcc failed:", err)
        os.Exit(1)
    }
    defer os.Remove(bin)

    // Запуск и сбор вывода
    out, err := exec.Command(bin).Output()
    if err != nil {
        fmt.Fprintln(os.Stderr, "gen binary failed:", err)
        os.Exit(1)
    }

    // Парсинг key = value
    consts := map[string]string{}
    for _, line := range strings.Split(string(out), "\n") {
        line = strings.TrimSpace(line)
        if line == "" { continue }
        parts := strings.SplitN(line, " = ", 2)
        if len(parts) == 2 {
            consts[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
        }
    }

    // Генерация Go-файла
    tmpl := template.Must(template.New("").Parse(goTemplate))
    var buf bytes.Buffer
    if err := tmpl.Execute(&buf, map[string]any{
        "Consts": consts,
        "Year":   time.Now().Year(),
    }); err != nil {
        fmt.Fprintln(os.Stderr, "template failed:", err)
        os.Exit(1)
    }

    out2 := filepath.Join(outDir, "reqrep_layout_gen.go")
    if err := os.WriteFile(out2, buf.Bytes(), 0644); err != nil {
        fmt.Fprintln(os.Stderr, "write failed:", err)
        os.Exit(1)
    }
    fmt.Println("generated", out2)
}

const goTemplate = `// Code generated by gen/gen.go from reqrep.h. DO NOT EDIT.

package reqrep

import "unsafe"

// Константы из reqrep.h
const (
{{- range $k, $v := .Consts}}
    layout{{$k}} = {{$v}}
{{- end}}
)

// Accessor-функции — компилятор инлайнит, bounds check отсутствует.
func hdrField32(base unsafe.Pointer, off uintptr) *uint32 {
    return (*uint32)(unsafe.Pointer(uintptr(base) + off))
}
func hdrField64(base unsafe.Pointer, off uintptr) *uint64 {
    return (*uint64)(unsafe.Pointer(uintptr(base) + off))
}
`
```

### Обновление vendored reqrep.h

```makefile
# Makefile в корне пакета
update-header:
    curl -fsSL https://raw.githubusercontent.com/vividsnow/perl5-data-reqrep-shared/master/reqrep.h \
        -o gen/reqrep.h
    go generate ./...
    git diff reqrep_layout_gen.go  # посмотреть что изменилось
```

### go:generate директива

В `reqrep.go`:
```go
//go:generate go run gen/gen.go
```

### Что генерируется, что пишется вручную

| Файл | Откуда |
|---|---|
| `reqrep_layout_gen.go` | **генерируется** из `reqrep.h` — смещения, размеры, константы |
| `reqrep_layout.go` | вручную — accessor-обёртки поверх сгенерированных констант |
| `reqrep.go`, `reqrep_*.go` | вручную — весь протокол и алгоритмы |
| `gen/reqrep.h` | вендоринг, обновляется через `make update-header` |

### Тест на соответствие layout

```go
// reqrep_layout_test.go — запускается отдельно, требует gcc
func TestLayoutMatchesC(t *testing.T) {
    // Компилирует gen_layout.c, запускает, сравнивает с layoutXxx константами.
    // Падает если расхождение > 0. Запускать в CI после make update-header.
}
```

---

## Файловая структура

```
reqrep/
  reqrep.go              — Server, Client, Query, Serve, Close
  reqrep_layout.go       — accessor-обёртки (используют константы из _gen.go)
  reqrep_layout_gen.go   — GENERATED: смещения, размеры, магические числа
  reqrep_futex_linux.go  — futexWait, futexWake, spin helper
  reqrep_mutex.go        — mutex lock/unlock (arena protection)
  reqrep_arena.go        — WriteArena, ReadArena, wraparound
  reqrep_slots.go        — AcquireRespSlot, ReleaseRespSlot, stale recovery
  reqrep_queue.go        — EnqueueRequest, DequeueRequest
  reqrep_test.go         — кросс-процессные тесты (os/exec + env var)
  reqrep_layout_test.go  — TestLayoutMatchesC (требует gcc)
  reqrep_bench_test.go   — бенчмарки
  gen/
    reqrep.h             — вендорная копия C-заголовка
    gen_layout.c         — C-программа, печатающая offsetof/sizeof
    gen.go               — //go:build ignore; генератор Go-файла
```

---

## Тестирование

### Кросс-языковые тесты (приоритет)

- Go-сервер + Perl-клиент: echo-handler, проверка корректности ответов
- Perl-сервер + Go-клиент: аналогично
- Файл создаётся Go-сервером, открывается Perl-клиентом и наоборот

### Функциональные Go-Go тесты

- echo-handler: `slices.Reverse(req)`, N запросов (10/100/1M)
- MPMC: 8 клиент-горутин × 4 сервер-горутины, 10K запросов, `-race`
- Stale recovery: SIGKILL клиента в середине запроса → сервер продолжает работу
- Cancel: `ctx` с таймаутом 1ms → `context.DeadlineExceeded`, generation check

### Бенчмарки

```
BenchmarkQuery/1KB
BenchmarkQuery/64KB
BenchmarkQuery/4MB
BenchmarkQueryParallel/N=4
BenchmarkQueryParallel/N=8
```

---

## Зависимости

- `golang.org/x/sys/unix` — `unix.Mmap`, `unix.Munmap`, `unix.Syscall6` (futex)
- Только stdlib + x/sys. Никакого CGo.

---

## Non-Goals

- Не сетевой транспорт (только одна машина)
- Нет шифрования (данные в /tmp)
- Нет динамического расширения capacity
- Int mode не реализовывать (несовместим с Perl string mode)
- Нет поддержки Windows/macOS

---

## Эталон для сверки

- Исходник C: https://raw.githubusercontent.com/vividsnow/perl5-data-reqrep-shared/master/reqrep.h
- Perl XS обёртка: https://raw.githubusercontent.com/vividsnow/perl5-data-reqrep-shared/master/Shared.xs
- Алгоритм Vyukov MPMC: https://www.1024cores.net/home/lock-free-algorithms/queues/bounded-mpmc-queue
