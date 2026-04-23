#include <stddef.h>
#include <stdio.h>
#include "reqrep.h"

int main(void) {
    /* Magic, version, mode constants */
    printf("Magic          = 0x%08X\n", REQREP_MAGIC);
    printf("Version        = %u\n",     REQREP_VERSION);
    printf("ModeStr        = %d\n",     REQREP_MODE_STR);
    printf("ModeInt        = %d\n",     REQREP_MODE_INT);

    /* State constants */
    printf("RespFree       = %d\n", RESP_FREE);
    printf("RespAcquired   = %d\n", RESP_ACQUIRED);
    printf("RespReady      = %d\n", RESP_READY);

    /* Flags and masks */
    printf("Utf8Flag       = 0x%08X\n", REQREP_UTF8_FLAG);
    printf("StrLenMask     = 0x%08X\n", REQREP_STR_LEN_MASK);
    printf("MutexWriterBit = 0x%08X\n", REQREP_MUTEX_WRITER_BIT);
    printf("MutexPidMask   = 0x%08X\n", REQREP_MUTEX_PID_MASK);
    printf("SpinLimit      = %d\n",     REQREP_SPIN_LIMIT);
    printf("LockTimeoutSec = %d\n",     REQREP_LOCK_TIMEOUT_SEC);

    /* Struct sizes */
    printf("SizeofHeader   = %zu\n", sizeof(ReqRepHeader));
    printf("SizeofReqSlot  = %zu\n", sizeof(ReqSlot));
    printf("SizeofRespSlot = %zu\n", sizeof(RespSlotHeader));

    /* ReqRepHeader field offsets */
    printf("HdrMagic          = %zu\n", offsetof(ReqRepHeader, magic));
    printf("HdrVersion        = %zu\n", offsetof(ReqRepHeader, version));
    printf("HdrMode           = %zu\n", offsetof(ReqRepHeader, mode));
    printf("HdrReqCap         = %zu\n", offsetof(ReqRepHeader, req_cap));
    printf("HdrTotalSize      = %zu\n", offsetof(ReqRepHeader, total_size));
    printf("HdrReqSlotsOff    = %zu\n", offsetof(ReqRepHeader, req_slots_off));
    printf("HdrReqArenaOff    = %zu\n", offsetof(ReqRepHeader, req_arena_off));
    printf("HdrReqArenaCap    = %zu\n", offsetof(ReqRepHeader, req_arena_cap));
    printf("HdrRespSlots      = %zu\n", offsetof(ReqRepHeader, resp_slots));
    printf("HdrRespDataMax    = %zu\n", offsetof(ReqRepHeader, resp_data_max));
    printf("HdrRespOff        = %zu\n", offsetof(ReqRepHeader, resp_off));
    printf("HdrRespStride     = %zu\n", offsetof(ReqRepHeader, resp_stride));
    printf("HdrReqHead        = %zu\n", offsetof(ReqRepHeader, req_head));
    printf("HdrRecvWaiters    = %zu\n", offsetof(ReqRepHeader, recv_waiters));
    printf("HdrRecvFutex      = %zu\n", offsetof(ReqRepHeader, recv_futex));
    printf("HdrReqTail        = %zu\n", offsetof(ReqRepHeader, req_tail));
    printf("HdrSendWaiters    = %zu\n", offsetof(ReqRepHeader, send_waiters));
    printf("HdrSendFutex      = %zu\n", offsetof(ReqRepHeader, send_futex));
    printf("HdrMutex          = %zu\n", offsetof(ReqRepHeader, mutex));
    printf("HdrMutexWaiters   = %zu\n", offsetof(ReqRepHeader, mutex_waiters));
    printf("HdrArenaWpos      = %zu\n", offsetof(ReqRepHeader, arena_wpos));
    printf("HdrArenaUsed      = %zu\n", offsetof(ReqRepHeader, arena_used));
    printf("HdrRespHint       = %zu\n", offsetof(ReqRepHeader, resp_hint));
    printf("HdrStatRecoveries = %zu\n", offsetof(ReqRepHeader, stat_recoveries));
    printf("HdrSlotFutex      = %zu\n", offsetof(ReqRepHeader, slot_futex));
    printf("HdrSlotWaiters    = %zu\n", offsetof(ReqRepHeader, slot_waiters));
    printf("HdrStatRequests   = %zu\n", offsetof(ReqRepHeader, stat_requests));
    printf("HdrStatReplies    = %zu\n", offsetof(ReqRepHeader, stat_replies));
    printf("HdrStatSendFull   = %zu\n", offsetof(ReqRepHeader, stat_send_full));
    printf("HdrStatRecvEmpty  = %zu\n", offsetof(ReqRepHeader, stat_recv_empty));

    /* ReqSlot field offsets */
    printf("ReqArenaOff  = %zu\n", offsetof(ReqSlot, arena_off));
    printf("ReqPackedLen = %zu\n", offsetof(ReqSlot, packed_len));
    printf("ReqArenaSkip = %zu\n", offsetof(ReqSlot, arena_skip));
    printf("ReqRespSlot  = %zu\n", offsetof(ReqSlot, resp_slot));
    printf("ReqRespGen   = %zu\n", offsetof(ReqSlot, resp_gen));

    /* RespSlotHeader field offsets */
    printf("RespState      = %zu\n", offsetof(RespSlotHeader, state));
    printf("RespWaiters    = %zu\n", offsetof(RespSlotHeader, waiters));
    printf("RespOwnerPid   = %zu\n", offsetof(RespSlotHeader, owner_pid));
    printf("RespLen        = %zu\n", offsetof(RespSlotHeader, resp_len));
    printf("RespFlags      = %zu\n", offsetof(RespSlotHeader, resp_flags));
    printf("RespGeneration = %zu\n", offsetof(RespSlotHeader, generation));
    printf("RespDataStart  = %zu\n", sizeof(RespSlotHeader));

    return 0;
}
