package main

import (
	"errors"
	"hash/fnv"
	"log"
	"strconv"
	"strings"
	"time"
)

type CmdType byte

const (
	COMMAND_UNKNOWN CmdType = 0x00
	COMMAND_INLINE  CmdType = 0x01
	COMMAND_BULK    CmdType = 0x02
)

const (
	GODIS_IO_BUF     int = 1024 * 16
	GODIS_MAX_BULK   int = 1024 * 4
	GODIS_MAX_INLINE int = 1024 * 4
)

type GodisDB struct {
	data   *Dict
	expire *Dict
}

type GodisServer struct {
	fd      int
	port    int
	db      *GodisDB
	clients map[int]*GodisClient
	aeLoop  *AeLoop
}

type GodisClient struct {
	fd       int
	db       *GodisDB
	args     []*Gobj
	reply    *List
	sentLen  int
	queryBuf []byte
	queryLen int
	cmdTy    CmdType
	bulkNum  int
	bulkLen  int
}

type CommandProc func(c *GodisClient)

// GodisCommand do not support bulk command
type GodisCommand struct {
	name  string
	proc  CommandProc
	arity int // command number
}

var server GodisServer

func expireIfNeeded(key *Gobj) {
	entry := server.db.expire.Find(key)
	if entry == nil {
		return
	}
	when := entry.Val.IntVal()
	if when > GetMsTime() {
		return
	}
	// key expired
	server.db.expire.Delete(key)
	server.db.data.Delete(key)
}

func findKeyRead(key *Gobj) *Gobj {
	// check out if the key expired
	expireIfNeeded(key)
	return server.db.data.Get(key)
}

func lookupCommand(cmdStr string) *GodisCommand {
	for _, c := range cmdTable {
		if c.name == cmdStr {
			return &c
		}
	}
	return nil
}

func (c *GodisClient) AddReply(o *Gobj) {
	c.reply.Append(o)
	o.IncrRefCount()
	server.aeLoop.AddFileEvent(c.fd, AE_WRITABLE, SendReplyToClient, c)
}

func (c *GodisClient) AddReplyStr(str string) {
	o := CreateObject(GSTR, str)
	c.AddReply(o)
	o.DecrRefCount()
}

func ProcessCommand(c *GodisClient) {
	cmdStr := c.args[0].StrVal()
	log.Printf("process command: %v\n", cmdStr)
	if cmdStr == "quit" {
		freeClient(c)
		return
	}
	cmd := lookupCommand(cmdStr)
	if cmd == nil {
		c.AddReplyStr("-ERR: unknown command\r\n")
		resetClient(c)
		return
	} else if cmd.arity != len(c.args) {
		c.AddReplyStr("-ERR: wrong number of args\r\n")
		resetClient(c)
		return
	}
	cmd.proc(c)
	resetClient(c)
}

func freeArgs(client *GodisClient) {
	for _, v := range client.args {
		v.DecrRefCount()
	}
}

func freeReplyList(client *GodisClient) {
	for client.reply.length != 0 {
		n := client.reply.head
		client.reply.DelNode(n)
		n.Val.DecrRefCount()
	}
}

func freeClient(client *GodisClient) {
	freeArgs(client)
	delete(server.clients, client.fd)
	server.aeLoop.RemoveFileEvent(client.fd, AE_READABLE)
	server.aeLoop.RemoveFileEvent(client.fd, AE_WRITABLE)
	freeReplyList(client)
	Close(client.fd)
}

func resetClient(client *GodisClient) {
	freeArgs(client)
	client.cmdTy = COMMAND_UNKNOWN
	client.bulkLen = 0
	client.bulkNum = 0
}

// 找到第一条命令
// 每条命令格式如下："xxxx\r\nxxxx\r\n"
func (client *GodisClient) findLineInQuery() (int, error) {
	idx := strings.Index(string(client.queryBuf[:client.queryLen]), "\r\n")
	if idx < 0 && client.queryLen > GODIS_MAX_INLINE {
		return idx, errors.New("too big inline cmd")
	}
	return idx, nil
}

func (client *GodisClient) getNumInQuery(st int, ed int) (int, error) {
	num, err := strconv.Atoi(string(client.queryBuf[st:ed]))
	client.queryBuf = client.queryBuf[ed+2:]
	client.queryLen -= ed + 2
	return num, err
}

func handleInlineBuf(client *GodisClient) (bool, error) {
	idx, err := client.findLineInQuery()
	if idx < 0 {
		return false, err
	}
	subs := strings.Split(string(client.queryBuf[:idx]), " ")
	client.queryBuf = client.queryBuf[idx+2:]
	client.queryLen -= idx + 2
	client.args = make([]*Gobj, len(subs))
	for i, v := range subs {
		client.args[i] = CreateObject(GSTR, v)
	}
	return true, nil
}

func handleBulkBuf(client *GodisClient) (bool, error) {
	// read bulk num
	if client.bulkNum == 0 {
		idx, err := client.findLineInQuery()
		if idx < 0 {
			return false, nil
		}
		bnum, err := client.getNumInQuery(1, idx)
		if err != nil {
			return false, nil
		}
		if bnum == 0 {
			return true, nil
		}
		client.bulkNum = bnum
		client.args = make([]*Gobj, bnum)
	}
	// read every bulk string
	for client.bulkNum > 0 {
		// read bulk length
		if client.bulkLen == 0 {
			idx, err := client.findLineInQuery()
			if idx < 0 {
				return false, nil
			}

			if client.queryBuf[0] != '$' {
				return false, errors.New("expect $ for bulk length")
			}

			blen, err := client.getNumInQuery(1, idx)
			if err != nil || blen == 0 {
				return false, err
			}

			if blen > GODIS_MAX_BULK {
				return false, errors.New("too big bulk")
			}
			client.bulkLen = blen
		}
		// read bulk string
		if client.queryLen < client.bulkLen+2 {
			return false, nil
		}
		idx := client.bulkLen
		if client.queryBuf[idx] != '\r' || client.queryBuf[idx+1] != '\n' {
			return false, errors.New("expect CRLF for bulk end")
		}
		client.args[len(client.args)-client.bulkNum] = CreateObject(GSTR, string(client.queryBuf[:idx]))
		client.queryBuf = client.queryBuf[idx+2:]
		client.queryLen -= idx + 2
		client.bulkLen = 0
		client.bulkNum -= 1
	}
	// complete reading every bulk
	return true, nil
}

func ProcessQueryBuf(client *GodisClient) error {
	for client.queryLen > 0 {
		if client.cmdTy == COMMAND_UNKNOWN {
			if client.queryBuf[0] == '*' {
				client.cmdTy = COMMAND_BULK
			} else {
				client.cmdTy = COMMAND_INLINE
			}
		}
		// transfer query -> args
		var ok bool
		var err error
		if client.cmdTy == COMMAND_INLINE {
			ok, err = handleInlineBuf(client)
		} else if client.cmdTy == COMMAND_BULK {
			ok, err = handleBulkBuf(client)
		} else {
			return errors.New("unknown Godis Command Type")
		}
		if err != nil {
			return err
		}
		// after query -> args
		if ok {
			if len(client.args) == 0 {
				resetClient(client)
			} else {
				ProcessCommand(client)
			}
		} else {
			// cmd incomplete
			break
		}
	}
	return nil
}

func ReadQueryFromClient(loop *AeLoop, fd int, extra interface{}) {
	client := extra.(*GodisClient)
	if len(client.queryBuf)-client.queryLen < GODIS_MAX_BULK {
		// resize query bulk
		client.queryBuf = append(client.queryBuf, make([]byte, GODIS_MAX_BULK)...)
	}
	n, err := Read(fd, client.queryBuf[client.queryLen:])
	if err != nil {
		log.Printf("client %v read err: %v\n", fd, err)
		freeClient(client)
		return
	}
	client.queryLen += n
	log.Printf("read %v bytes from client: %v\n", n, client.fd)
	log.Printf("ReadQueryFrom Client, queryBuf: %v\n", string(client.queryBuf))
	err = ProcessQueryBuf(client)
	if err != nil {
		log.Printf("process query buf err: %v\n", err)
		freeClient(client)
		return
	}
}

func SendReplyToClient(loop *AeLoop, fd int, extra interface{}) {
	client := extra.(*GodisClient)
	log.Printf("Send Reply to Client, reply len: %v\n", client.reply.Length())
	for client.reply.Length() > 0 {
		rep := client.reply.First()
		buf := []byte(rep.Val.StrVal())
		bufLen := len(buf)
		if client.sentLen < bufLen {
			n, err := Write(fd, buf[client.sentLen:])
			if err != nil {
				log.Printf("send reply err: %v\n", err)
				freeClient(client)
				return
			}
			client.sentLen += n
			log.Printf("send %v bytes reply to client: %v\n", n, client.fd)
			if client.sentLen == bufLen {
				client.reply.DelNode(rep)
				rep.Val.DecrRefCount()
				client.sentLen = 0
			} else {
				break
			}
		}
	}
	if client.reply.Length() == 0 {
		client.sentLen = 0
		loop.RemoveFileEvent(fd, AE_WRITABLE)
	}
}

func GStrEqual(a, b *Gobj) bool {
	if a.Type_ != GSTR || b.Type_ != GSTR {
		return false
	}
	return a.StrVal() == b.StrVal()
}

func GStrHash(key *Gobj) int64 {
	if key.Type_ != GSTR {
		return 0
	}
	hash := fnv.New64()
	hash.Write([]byte(key.StrVal()))
	return int64(hash.Sum64())
}

func CreateClient(fd int) *GodisClient {
	var client GodisClient
	client.fd = fd
	client.db = server.db
	client.queryBuf = make([]byte, GODIS_IO_BUF)
	client.reply = ListCreate(ListType{EqualFunc: GStrEqual})
	return &client
}

func AcceptHandler(loop *AeLoop, fd int, extra interface{}) {
	cfd, err := Accept(fd)
	if err != nil {
		log.Printf("accept err: %v\n", err)
		return
	}
	client := CreateClient(cfd)
	// todo: check max client limit
	server.clients[cfd] = client
	server.aeLoop.AddFileEvent(cfd, AE_READABLE, ReadQueryFromClient, client)
	log.Printf("accept client, fd: %d\n", cfd)
}

const EXPIRE_CHECK_COUNT int = 100

// ServerCron background job, runs every 100ms
func ServerCron(loop *AeLoop, id int, extra interface{}) {
	for i := 0; i < EXPIRE_CHECK_COUNT; i++ {
		entry := server.db.expire.RandomGet()
		if entry == nil {
			break
		}
		if entry.Val.IntVal() < time.Now().Unix() {
			server.db.data.Delete(entry.Key)
			server.db.expire.Delete(entry.Key)
		}
	}
}

func initServer(config *Config) error {
	server.port = config.Port
	server.clients = make(map[int]*GodisClient)
	server.db = &GodisDB{
		data:   DictCreate(DictType{HashFunc: GStrHash, EqualFunc: GStrEqual}),
		expire: DictCreate(DictType{HashFunc: GStrHash, EqualFunc: GStrEqual}),
	}
	var err error
	if server.aeLoop, err = AeLoopCreate(); err != nil {
		return err
	}
	server.fd, err = TcpServer(server.port)
	return err
}

func main() {
	// cmd start
	//path := os.Args[1]
	path := "./config.json"
	config, err := LoadConfig(path)
	if err != nil {
		log.Printf("config error: %v\n", err)
		panic("load config error.")
	}
	err = initServer(config)
	if err != nil {
		log.Printf("init server error: %v\n", err)
		panic("init server error.")
	}
	server.aeLoop.AddFileEvent(server.fd, AE_READABLE, AcceptHandler, nil)
	server.aeLoop.AddTimeEvent(AE_NORMAL, 100, ServerCron, nil)
	log.Println("godis server is up.")
	PrintBanner()
	server.aeLoop.AeMain()
}
