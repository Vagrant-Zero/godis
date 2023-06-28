package main

import "fmt"

var cmdTable []GodisCommand = []GodisCommand{
	GodisCommand{name: "get", proc: getCommand, arity: 2},
	GodisCommand{name: "set", proc: setCommand, arity: 3},
	GodisCommand{name: "expire", proc: expireCommand, arity: 3},
	GodisCommand{name: "lpush", proc: lpushCommand, arity: 3},
	GodisCommand{name: "lpop", proc: lpopCommand, arity: 2},
	// todo: other commands
}

func getCommand(c *GodisClient) {
	key := c.args[1]
	val := findKeyRead(key)
	if val == nil {
		c.AddReplyStr("$-1\r\n")
	} else if val.Type_ != GSTR {
		c.AddReplyStr("-ERR: wrong type\r\n")
	} else {
		str := val.StrVal()
		c.AddReplyStr(fmt.Sprintf("$%d%v\r\n", len(str), str))
	}
}

func setCommand(c *GodisClient) {
	key := c.args[1]
	val := c.args[2]
	if val.Type_ != GSTR {
		c.AddReplyStr("-ERR: wrong type\r\n")
	}
	// checkout key-val type is correct or not
	entry := server.db.data.Find(key)
	if entry != nil && entry.Val.Type_ != GSTR {
		c.AddReplyStr(fmt.Sprintf("%v\r\n", WO_ERR.Error()))
		return
	}
	// set key-val pair
	server.db.data.Set(key, val)
	server.db.expire.Delete(key)
	c.AddReplyStr("+OK\r\n")
}

func expireCommand(c *GodisClient) {
	key := c.args[1]
	val := c.args[2]
	// todo : extract same code, use methods like aop
	if val.Type_ != GSTR {
		c.AddReplyStr("-ERR: wrong type\r\n")
	}
	expire := GetMsTime() + (val.IntVal() * 1000)
	expObj := CreateFromInt(expire)
	server.db.expire.Set(key, expObj)
	expObj.DecrRefCount()
	c.AddReplyStr("+OK\r\n")
}

func lpushCommand(c *GodisClient) {
	key := c.args[1]
	val := c.args[2]
	if val.Type_ != GSTR {
		c.AddReplyStr("-ERR: wrong type\r\n")
	}
	err := lPush(server.db.data, key, val)
	if err != nil {
		c.AddReplyStr(fmt.Sprintf("%v\r\n", err.Error()))
		return
	}
	server.db.expire.Delete(key)
	c.AddReplyStr("+OK\r\n")
}

func lPush(dict *Dict, key *Gobj, val *Gobj) error {
	entry := dict.Find(key)
	if entry != nil {
		if entry.Val.Type_ != GLIST {
			return WO_ERR
		}
		ls := entry.Val.Val_.(*List)
		ls.LPush(val)
		val.IncrRefCount()
		return nil
	}
	entry = dict.AddRaw(key)
	ls := ListCreate(ListType{EqualFunc: GStrEqual})
	entry.Val = CreateObject(GLIST, ls)
	ls.LPush(val)
	val.IncrRefCount()
	return nil
}

func lpopCommand(c *GodisClient) {
	key := c.args[1]
	// checkout key either exists
	val := findKeyRead(key)
	if val == nil {
		c.AddReplyStr("$-1\r\n")
		return
	}
	ls := val.Val_.(*List)
	node := ls.LPop()
	if node == nil {
		c.AddReplyStr("$-1\r\n")
		return
	}
	str := node.StrVal()
	c.AddReplyStr(fmt.Sprintf("$%d%v\r\n", len(str), str))
}
