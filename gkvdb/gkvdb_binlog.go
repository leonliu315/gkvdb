package gkvdb

import (
    "os"
    "sync"
    "errors"
    "gitee.com/johng/gf/g/os/gfile"
    "gitee.com/johng/gf/g/os/gfilepool"
    "gitee.com/johng/gf/g/container/glist"
    "gitee.com/johng/gf/g/encoding/gbinary"
)

// binlog操作对象
type BinLog struct {
    sync.RWMutex
    db    *DB
    fp    *gfilepool.Pool
    queue *glist.SafeList
}

// binlog写入项
type BinLogItem struct {
    txstart int64                        // 事务在binlog文件的开始位置
    datamap map[string]map[string][]byte // 事务数据
}

// 创建binlog对象
func newBinLog(db *DB) (*BinLog, error) {
    binlog := &BinLog{
        db    : db,
        queue : glist.NewSafeList(),
    }
    path := db.getBinLogFilePath()
    if gfile.Exists(path) && (!gfile.IsWritable(path) || !gfile.IsReadable(path)){
        return nil, errors.New("permission denied to binlog file: " + path)
    }
    binlog.fp = gfilepool.New(path, os.O_RDWR|os.O_CREATE, gFILE_POOL_CACHE_TIMEOUT)
    return binlog, nil
}

// 关闭binlog
func (binlog *BinLog) close() {
    binlog.fp.Close()
}

// 从binlog文件中恢复未同步数据到memtable中
// 内部会检测异常数据写入，并忽略异常数据，以便异常数据不会进入到数据库中
func (binlog *BinLog) initFromFile() {
    //binlog.RLock()
    //blbuffer := gfile.GetBinContents(binlog.db.getBinLogFilePath())
    //binlog.RUnlock()
    //
    //if len(blbuffer) == 0 {
    //    return
    //}
    //t1 := gtime.Microsecond()
    //// 在异常数据下，需要花费更多的时间进行数据纠正(字节不断递增计算下一条正确的binlog位置)
    //for i := 0; i < len(blbuffer); {
    //    buffer := blbuffer[i : i + 13]
    //    synced := int(gbinary.DecodeToInt8(buffer[0 : 1]))
    //    blsize := int(gbinary.DecodeToInt32(buffer[1 : 5]))
    //    if i + 13 + blsize + 8 > len(blbuffer) {
    //        i++
    //        continue
    //    }
    //    txidstart := gbinary.DecodeToInt64(buffer[5 : 13])
    //    txidend   := gbinary.DecodeToInt64(blbuffer[i + 13 + blsize : i + 13 + blsize + 8])
    //    if txidstart != txidend {
    //        fmt.Println("invalid", i)
    //        i++
    //        continue
    //    } else {
    //        // 正常数据，同步到memtable中
    //        if synced == 0 {
    //            datamap := binlog.binlogBufferToDataMap(blbuffer[i + 13 : i + 13 + blsize])
    //            binlog.queue.PushFront(BinLogItem{int64(i), datamap})
    //            binlog.db.memt.set(datamap)
    //        }
    //        i += 13 + blsize + 8
    //    }
    //}
    //fmt.Println(gtime.Microsecond() - t1)
}

// 将二进制数据转换为事务对象
func (binlog *BinLog) binlogBufferToDataMap(buffer []byte) map[string][]byte {
    m := make(map[string][]byte)
    for i := 0; i < len(buffer); {
        bits  := gbinary.DecodeBytesToBits(buffer[i : i + 4])
        klen  := int(gbinary.DecodeBits(bits[0 : 8]))
        vlen  := int(gbinary.DecodeBits(bits[8 : 32]))
        key   := buffer[i + 4 : i + 4 + klen]
        value := buffer[i + 4 + klen : i + 4 + klen + vlen]
        m[string(key)] = value
        i += 4 + klen + vlen
    }
    return m
}

// 添加binlog到文件，支持批量添加
// 返回写入的文件开始位置，以及是否有错误
func (binlog *BinLog) writeByTx(tx *Transaction) error {
    buffer := make([]byte, 0)
    // 事务开始
    buffer  = append(buffer, gbinary.EncodeInt8(0)...)
    buffer  = append(buffer, gbinary.EncodeInt32(0)...)
    buffer  = append(buffer, gbinary.EncodeInt64(tx.id)...)
    // 数据列表
    blsize := 0
    for n, m := range tx.tables {
        for ks, v := range m {
            k      := []byte(ks)
            bits   := make([]gbinary.Bit, 0)
            bits    = gbinary.EncodeBits(bits, uint(len(n)),   8)
            bits    = gbinary.EncodeBits(bits, uint(len(k)),   8)
            bits    = gbinary.EncodeBits(bits, uint(len(v)),  24)
            buffer  = append(buffer, gbinary.EncodeBitsToBytes(bits)...)
            buffer  = append(buffer, n...)
            buffer  = append(buffer, k...)
            buffer  = append(buffer, v...)
            blsize += 4 + len(k) + len(v)
        }
    }

    // 事务结束
    buffer  = append(buffer, gbinary.EncodeInt64(tx.id)...)
    // 修改数据长度
    copy(buffer[1:], gbinary.EncodeInt32(int32(blsize)))

    // 从指针池获取
    blpf, err := binlog.fp.File()
    if err != nil {
        return err
    }
    defer blpf.Close()

    binlog.Lock()
    defer binlog.Unlock()

    // 写到文件末尾
    start, err := blpf.File().Seek(0, 2)
    if err != nil {
        return err
    }
    // 执行数据写入
    if _, err := blpf.File().WriteAt(buffer, start); err != nil {
        return err
    }

    // 添加到磁盘化队列
    binlog.queue.PushFront(BinLogItem{start, tx.tables})

    return nil
}

// 写入磁盘，标识事务已经同步，在对应位置只写入1个字节
func (binlog *BinLog) markTxSynced(start int64) error {
    blpf, err := binlog.fp.File()
    if err != nil {
        return err
    }
    defer blpf.Close()

    binlog.Lock()
    defer binlog.Unlock()

    if _, err := blpf.File().WriteAt(gbinary.EncodeInt8(1), start); err != nil {
        return err
    }
    return nil
}
