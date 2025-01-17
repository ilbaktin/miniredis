// Commands from https://redis.io/commands#stream

package miniredis

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alicebob/miniredis/v2/server"
)

// commandsStream handles all stream operations.
func commandsStream(m *Miniredis) {
	m.srv.Register("XADD", m.cmdXadd)
	m.srv.Register("XLEN", m.cmdXlen)
	m.srv.Register("XREAD", m.cmdXread)
	m.srv.Register("XRANGE", m.makeCmdXrange(false))
	m.srv.Register("XREVRANGE", m.makeCmdXrange(true))
	m.srv.Register("XGROUP", m.cmdXgroup)
	m.srv.Register("XINFO", m.cmdXinfo)
	m.srv.Register("XREADGROUP", m.cmdXreadgroup)
	m.srv.Register("XACK", m.cmdXack)
	m.srv.Register("XDEL", m.cmdXdel)
	m.srv.Register("XPENDING", m.cmdXpending)
}

// XADD
func (m *Miniredis) cmdXadd(c *server.Peer, cmd string, args []string) {
	if len(args) < 4 {
		setDirty(c)
		c.WriteError(errWrongNumber(cmd))
		return
	}
	if !m.handleAuth(c) {
		return
	}
	if m.checkPubsub(c, cmd) {
		return
	}

	key, args := args[0], args[1:]

	withTx(m, c, func(c *server.Peer, ctx *connCtx) {

		maxlen := -1
		if strings.ToLower(args[0]) == "maxlen" {
			args = args[1:]
			// we don't treat "~" special
			if args[0] == "~" {
				args = args[1:]
			}
			n, err := strconv.Atoi(args[0])
			if err != nil {
				c.WriteError(msgInvalidInt)
				return
			}
			if n < 0 {
				c.WriteError("ERR The MAXLEN argument must be >= 0.")
				return
			}
			maxlen = n
			args = args[1:]
		}
		if len(args) < 1 {
			c.WriteError(errWrongNumber(cmd))
			return
		}
		entryID, args := args[0], args[1:]

		// args must be composed of field/value pairs.
		if len(args) == 0 || len(args)%2 != 0 {
			c.WriteError("ERR wrong number of arguments for XADD") // non-default message
			return
		}

		var values []string
		for len(args) > 0 {
			values = append(values, args[0], args[1])
			args = args[2:]
		}

		db := m.db(ctx.selectedDB)
		s, err := db.stream(key)
		if err != nil {
			c.WriteError(err.Error())
			return
		}
		if s == nil {
			// TODO: NOMKSTREAM
			s, _ = db.newStream(key)
		}

		newID, err := s.add(entryID, values, m.effectiveNow())
		if err != nil {
			switch err {
			case errInvalidEntryID:
				c.WriteError(msgInvalidStreamID)
			default:
				c.WriteError(err.Error())
			}
			return
		}
		if maxlen >= 0 {
			s.trim(maxlen)
		}
		db.keyVersion[key]++

		c.WriteBulk(newID)
	})
}

// XLEN
func (m *Miniredis) cmdXlen(c *server.Peer, cmd string, args []string) {
	if len(args) != 1 {
		setDirty(c)
		c.WriteError(errWrongNumber(cmd))
		return
	}
	if !m.handleAuth(c) {
		return
	}
	if m.checkPubsub(c, cmd) {
		return
	}

	key := args[0]

	withTx(m, c, func(c *server.Peer, ctx *connCtx) {
		db := m.db(ctx.selectedDB)

		s, err := db.stream(key)
		if err != nil {
			c.WriteError(err.Error())
		}
		if s == nil {
			// No such key. That's zero length.
			c.WriteInt(0)
			return
		}

		c.WriteInt(len(s.entries))
	})
}

// XRANGE and XREVRANGE
func (m *Miniredis) makeCmdXrange(reverse bool) server.Cmd {
	return func(c *server.Peer, cmd string, args []string) {
		if len(args) < 3 {
			setDirty(c)
			c.WriteError(errWrongNumber(cmd))
			return
		}
		if len(args) == 4 || len(args) > 5 {
			setDirty(c)
			c.WriteError(msgSyntaxError)
			return
		}
		if !m.handleAuth(c) {
			return
		}
		if m.checkPubsub(c, cmd) {
			return
		}

		var (
			key      = args[0]
			startKey = args[1]
			endKey   = args[2]
		)

		countArg := "0"
		if len(args) == 5 {
			if strings.ToLower(args[3]) != "count" {
				setDirty(c)
				c.WriteError(msgSyntaxError)
				return
			}
			countArg = args[4]
		}

		withTx(m, c, func(c *server.Peer, ctx *connCtx) {
			start, err := formatStreamRangeBound(startKey, true, reverse)
			if err != nil {
				c.WriteError(msgInvalidStreamID)
				return
			}
			end, err := formatStreamRangeBound(endKey, false, reverse)
			if err != nil {
				c.WriteError(msgInvalidStreamID)
				return
			}
			count, err := strconv.Atoi(countArg)
			if err != nil {
				c.WriteError(msgInvalidInt)
				return
			}

			db := m.db(ctx.selectedDB)

			if !db.exists(key) {
				c.WriteLen(0)
				return
			}

			if db.t(key) != "stream" {
				c.WriteError(ErrWrongType.Error())
				return
			}

			var entries = db.streamKeys[key].entries
			if reverse {
				entries = reversedStreamEntries(entries)
			}
			if count == 0 {
				count = len(entries)
			}

			var returnedEntries []StreamEntry
			for _, entry := range entries {
				if len(returnedEntries) == count {
					break
				}

				if !reverse {
					// Break if entry ID > end
					if streamCmp(entry.ID, end) == 1 {
						break
					}

					// Continue if entry ID < start
					if streamCmp(entry.ID, start) == -1 {
						continue
					}
				} else {
					// Break if entry iD < end
					if streamCmp(entry.ID, end) == -1 {
						break
					}

					// Continue if entry ID > start.
					if streamCmp(entry.ID, start) == 1 {
						continue
					}
				}

				returnedEntries = append(returnedEntries, entry)
			}

			c.WriteLen(len(returnedEntries))
			for _, entry := range returnedEntries {
				c.WriteLen(2)
				c.WriteBulk(entry.ID)
				c.WriteLen(len(entry.Values))
				for _, v := range entry.Values {
					c.WriteBulk(v)
				}
			}
		})
	}
}

// XGROUP
func (m *Miniredis) cmdXgroup(c *server.Peer, cmd string, args []string) {
	if (len(args) == 4 || len(args) == 5) && strings.ToUpper(args[0]) == "CREATE" {
		m.cmdXgroupCreate(c, cmd, args)
	} else {
		j := strings.Join(args, " ")
		err := fmt.Sprintf("ERR 'XGROUP %s' not supported", j)
		setDirty(c)
		c.WriteError(err)
	}
}

// XGROUP CREATE
func (m *Miniredis) cmdXgroupCreate(c *server.Peer, cmd string, args []string) {
	stream, group, id := args[1], args[2], args[3]

	withTx(m, c, func(c *server.Peer, ctx *connCtx) {
		db := m.db(ctx.selectedDB)

		s, err := db.stream(stream)
		if err != nil {
			c.WriteError(err.Error())
			return
		}
		if s == nil && len(args) == 5 && strings.ToUpper(args[4]) == "MKSTREAM" {
			if s, err = db.newStream(stream); err != nil {
				c.WriteError(err.Error())
				return
			}
		}
		if s == nil {
			c.WriteError(msgXgroupKeyNotFound)
			return
		}

		if err := s.createGroup(group, id); err != nil {
			c.WriteError(err.Error())
			return
		}

		c.WriteOK()
	})
}

// XINFO
func (m *Miniredis) cmdXinfo(c *server.Peer, cmd string, args []string) {
	if len(args) < 1 {
		setDirty(c)
		c.WriteError(errWrongNumber(cmd))
		return
	}
	switch strings.ToUpper(args[0]) {
	case "STREAM":
		m.cmdXinfoStream(c, args[1:])
	case "CONSUMERS", "GROUPS", "HELP":
		err := fmt.Sprintf("'XINFO %s' not supported", strings.Join(args, " "))
		setDirty(c)
		c.WriteError(err)
	default:
		setDirty(c)
		c.WriteError("ERR syntax error, try 'XINFO HELP'")
	}

}

// XINFO STREAM
// Produces only part of full command output
func (m *Miniredis) cmdXinfoStream(c *server.Peer, args []string) {
	if len(args) < 1 {
		setDirty(c)
		c.WriteError(errWrongNumber("XINFO"))
		return
	}
	key := args[0]
	withTx(m, c, func(c *server.Peer, ctx *connCtx) {
		db := m.db(ctx.selectedDB)

		s, err := db.stream(key)
		if err != nil {
			c.WriteError(err.Error())
			return
		}
		if s == nil {
			c.WriteError(msgKeyNotFound)
			return
		}

		c.WriteMapLen(1)
		c.WriteBulk("length")
		c.WriteInt(len(s.entries))
	})
}

// XREADGROUP
func (m *Miniredis) cmdXreadgroup(c *server.Peer, cmd string, args []string) {
	// XREADGROUP GROUP group consumer STREAMS key ID
	if len(args) < 6 {
		setDirty(c)
		c.WriteError(errWrongNumber(cmd))
		return
	}

	var opts struct {
		group        string
		consumer     string
		count        int
		noack        bool
		streams      []string
		ids          []string
		block        bool
		blockTimeout time.Duration
	}

	if strings.ToUpper(args[0]) != "GROUP" {
		setDirty(c)
		c.WriteError(msgSyntaxError)
		return
	}

	opts.group, opts.consumer, args = args[1], args[2], args[3:]

	var err error
parsing:
	for len(args) > 0 {
		switch strings.ToUpper(args[0]) {
		case "COUNT":
			if len(args) < 2 {
				err = errors.New(errWrongNumber(cmd))
				break parsing
			}

			opts.count, err = strconv.Atoi(args[1])
			if err != nil {
				break parsing
			}

			args = args[2:]
		case "BLOCK":
			err = parseBlock(cmd, args, &opts.block, &opts.blockTimeout)
			if err != nil {
				break parsing
			}
			args = args[2:]
		case "NOACK":
			args = args[1:]
			opts.noack = true
		case "STREAMS":
			args = args[1:]

			if len(args)%2 != 0 {
				err = errors.New(msgXreadUnbalanced)
				break parsing
			}

			opts.streams, opts.ids = args[0:len(args)/2], args[len(args)/2:]
			break parsing
		default:
			err = fmt.Errorf("ERR incorrect argument %s", args[0])
			break parsing
		}
	}

	if err != nil {
		setDirty(c)
		c.WriteError(err.Error())
		return
	}

	if len(opts.streams) == 0 || len(opts.ids) == 0 {
		setDirty(c)
		c.WriteError(errWrongNumber(cmd))
		return
	}

	for _, id := range opts.ids {
		if id != `>` {
			opts.block = false
		}
	}

	if !opts.block {
		withTx(m, c, func(c *server.Peer, ctx *connCtx) {
			db := m.db(ctx.selectedDB)
			res, err := xreadgroup(
				db,
				opts.group,
				opts.consumer,
				opts.noack,
				opts.streams,
				opts.ids,
				opts.count,
				m.effectiveNow(),
			)
			if err != nil {
				c.WriteError(err.Error())
				return
			}
			writeXread(c, opts.streams, res)
		})
		return
	}

	blocking(
		m,
		c,
		opts.blockTimeout,
		func(c *server.Peer, ctx *connCtx) bool {
			db := m.db(ctx.selectedDB)
			res, err := xreadgroup(
				db,
				opts.group,
				opts.consumer,
				opts.noack,
				opts.streams,
				opts.ids,
				opts.count,
				m.effectiveNow(),
			)
			if err != nil {
				c.WriteError(err.Error())
				return true
			}
			if len(res) == 0 {
				return false
			}
			writeXread(c, opts.streams, res)
			return true
		},
		func(c *server.Peer) { // timeout
			c.WriteLen(-1)
		},
	)
}

func xreadgroup(
	db *RedisDB,
	group,
	consumer string,
	noack bool,
	streams []string,
	ids []string,
	count int,
	now time.Time,
) (map[string][]StreamEntry, error) {
	res := map[string][]StreamEntry{}
	for i, key := range streams {
		id := ids[i]

		g, err := db.streamGroup(key, group)
		if err != nil {
			return nil, err
		}
		if g == nil {
			return nil, errXreadgroup(key, group)
		}

		if _, err := parseStreamID(id); id != `>` && err != nil {
			return nil, err
		}
		entries := g.readGroup(now, consumer, id, count, noack)
		if id == `>` && len(entries) == 0 {
			continue
		}

		res[key] = entries
	}
	return res, nil
}

// XACK
func (m *Miniredis) cmdXack(c *server.Peer, cmd string, args []string) {
	if len(args) < 3 {
		setDirty(c)
		c.WriteError(errWrongNumber(cmd))
		return
	}

	key, group, ids := args[0], args[1], args[2:]

	withTx(m, c, func(c *server.Peer, ctx *connCtx) {
		db := m.db(ctx.selectedDB)
		g, err := db.streamGroup(key, group)
		if err != nil {
			c.WriteError(err.Error())
			return
		}
		if g == nil {
			c.WriteInt(0)
			return
		}

		cnt, err := g.ack(ids)
		if err != nil {
			c.WriteError(err.Error())
			return
		}
		c.WriteInt(cnt)
	})
}

// XDEL
func (m *Miniredis) cmdXdel(c *server.Peer, cmd string, args []string) {
	if len(args) < 2 {
		setDirty(c)
		c.WriteError(errWrongNumber(cmd))
		return
	}

	stream, ids := args[0], args[1:]

	withTx(m, c, func(c *server.Peer, ctx *connCtx) {
		db := m.db(ctx.selectedDB)
		s, err := db.stream(stream)
		if err != nil {
			c.WriteError(err.Error())
			return
		}
		if s == nil {
			c.WriteInt(0)
			return
		}

		n, err := s.delete(ids)
		if err != nil {
			c.WriteError(err.Error())
			return
		}
		db.keyVersion[stream]++
		c.WriteInt(n)
	})
}

// XREAD
func (m *Miniredis) cmdXread(c *server.Peer, cmd string, args []string) {
	if len(args) < 3 {
		setDirty(c)
		c.WriteError(errWrongNumber(cmd))
		return
	}

	var opts struct {
		count        int
		streams      []string
		ids          []string
		block        bool
		blockTimeout time.Duration
	}
	var err error

parsing:
	for len(args) > 0 {
		switch strings.ToUpper(args[0]) {
		case "COUNT":
			if len(args) < 2 {
				err = errors.New(errWrongNumber(cmd))
				break parsing
			}

			opts.count, err = strconv.Atoi(args[1])
			if err != nil {
				break parsing
			}
			args = args[2:]
		case "BLOCK":
			err = parseBlock(cmd, args, &opts.block, &opts.blockTimeout)
			if err != nil {
				break parsing
			}
			args = args[2:]
		case "STREAMS":
			args = args[1:]

			if len(args)%2 != 0 {
				err = errors.New(msgXreadUnbalanced)
				break parsing
			}

			opts.streams, opts.ids = args[0:len(args)/2], args[len(args)/2:]
			for _, id := range opts.ids {
				if _, err := parseStreamID(id); err != nil {
					setDirty(c)
					c.WriteError(msgInvalidStreamID)
					return
				}
			}
			args = nil
			break parsing
		default:
			err = fmt.Errorf("ERR incorrect argument %s", args[0])
			break parsing
		}
	}

	if err != nil {
		setDirty(c)
		c.WriteError(err.Error())
		return
	}

	if !opts.block {
		withTx(m, c, func(c *server.Peer, ctx *connCtx) {
			db := m.db(ctx.selectedDB)
			res := xread(db, opts.streams, opts.ids, opts.count)
			writeXread(c, opts.streams, res)
		})
		return
	}
	blocking(
		m,
		c,
		opts.blockTimeout,
		func(c *server.Peer, ctx *connCtx) bool {
			db := m.db(ctx.selectedDB)
			res := xread(db, opts.streams, opts.ids, opts.count)
			if len(res) == 0 {
				return false
			}
			writeXread(c, opts.streams, res)
			return true
		},
		func(c *server.Peer) { // timeout
			c.WriteLen(-1)
		},
	)
}

func xread(db *RedisDB, streams []string, ids []string, count int) map[string][]StreamEntry {
	res := map[string][]StreamEntry{}
	for i := range streams {
		stream := streams[i]
		id := ids[i]

		var s, ok = db.streamKeys[stream]
		if !ok {
			continue
		}
		entries := s.entries
		if len(entries) == 0 {
			continue
		}

		entryCount := count
		if entryCount == 0 {
			entryCount = len(entries)
		}

		var returnedEntries []StreamEntry
		for _, entry := range entries {
			if len(returnedEntries) == entryCount {
				break
			}
			if streamCmp(entry.ID, id) <= 0 {
				continue
			}
			returnedEntries = append(returnedEntries, entry)
		}
		if len(returnedEntries) > 0 {
			res[stream] = returnedEntries
		}
	}
	return res
}

func writeXread(c *server.Peer, streams []string, res map[string][]StreamEntry) {
	if len(res) == 0 {
		c.WriteLen(-1)
		return
	}
	c.WriteLen(len(res))
	for _, stream := range streams {
		entries, ok := res[stream]
		if !ok {
			continue
		}
		c.WriteLen(2)
		c.WriteBulk(stream)
		c.WriteLen(len(entries))
		for _, entry := range entries {
			c.WriteLen(2)
			c.WriteBulk(entry.ID)
			c.WriteLen(len(entry.Values))
			for _, v := range entry.Values {
				c.WriteBulk(v)
			}
		}
	}
}

// XPENDING
func (m *Miniredis) cmdXpending(c *server.Peer, cmd string, args []string) {
	if len(args) < 2 {
		setDirty(c)
		c.WriteError(errWrongNumber(cmd))
		return
	}

	key, group, args := args[0], args[1], args[2:]
	summary := true
	if len(args) > 0 && strings.ToUpper(args[0]) == "IDLE" {
		setDirty(c)
		c.WriteError("ERR IDLE is unsupported")
		return
	}
	var (
		start, end string
		count      int
		consumer   *string
	)
	if len(args) >= 3 {
		summary = false

		start_, err := formatStreamRangeBound(args[0], true, false)
		if err != nil {
			c.WriteError(msgInvalidStreamID)
			return
		}
		start = start_
		end_, err := formatStreamRangeBound(args[1], false, false)
		if err != nil {
			c.WriteError(msgInvalidStreamID)
			return
		}
		end = end_
		n, err := strconv.Atoi(args[2]) // negative is allowed
		if err != nil {
			c.WriteError(msgInvalidInt)
			return
		}
		count = n
		args = args[3:]

		if len(args) == 1 {
			var c string
			c, args = args[0], args[1:]
			consumer = &c
		}
	}
	if len(args) != 0 {
		setDirty(c)
		c.WriteError(msgSyntaxError)
		return
	}

	withTx(m, c, func(c *server.Peer, ctx *connCtx) {
		db := m.db(ctx.selectedDB)
		g, err := db.streamGroup(key, group)
		if err != nil {
			c.WriteError(err.Error())
			return
		}
		if g == nil {
			c.WriteError(errReadgroup(key, group).Error())
			return
		}

		if summary {
			writeXpendingSummary(c, *g)
			return
		}
		writeXpending(m.effectiveNow(), c, *g, start, end, count, consumer)
	})
}

func writeXpendingSummary(c *server.Peer, g streamGroup) {
	if len(g.pending) == 0 {
		c.WriteLen(4)
		c.WriteInt(0)
		c.WriteNull()
		c.WriteNull()
		c.WriteLen(-1)
		return
	}

	// format:
	//  - number of pending
	//  - smallest ID
	//  - highest ID
	//  - all consumers with > 0 pending items
	c.WriteLen(4)
	c.WriteInt(len(g.pending))
	c.WriteBulk(g.pending[0].id)
	c.WriteBulk(g.pending[len(g.pending)-1].id)
	cons := map[string]int{}
	for id := range g.consumers {
		cnt := g.pendingCount(id)
		if cnt > 0 {
			cons[id] = cnt
		}
	}
	c.WriteLen(len(cons))
	var ids []string
	for id := range cons {
		ids = append(ids, id)
	}
	sort.Strings(ids) // be predicatable
	for _, id := range ids {
		c.WriteLen(2)
		c.WriteBulk(id)
		c.WriteBulk(strconv.Itoa(cons[id]))
	}
}

func writeXpending(
	now time.Time,
	c *server.Peer,
	g streamGroup,
	start,
	end string,
	count int,
	consumer *string,
) {
	if len(g.pending) == 0 || count < 0 {
		c.WriteLen(-1)
		return
	}

	// format, list of:
	//  - message ID
	//  - consumer
	//  - milliseconds since delivery
	//  - delivery count
	type entry struct {
		id       string
		consumer string
		millis   int
		count    int
	}
	var res []entry
	for _, p := range g.pending {
		if len(res) >= count {
			break
		}
		if consumer != nil && p.consumer != *consumer {
			continue
		}
		if streamCmp(p.id, start) < 0 {
			continue
		}
		if streamCmp(p.id, end) > 0 {
			continue
		}
		res = append(res, entry{
			id:       p.id,
			consumer: p.consumer,
			millis:   int(now.Sub(p.lastDelivery).Milliseconds()),
			count:    p.deliveryCount,
		})
	}
	c.WriteLen(len(res))
	for _, e := range res {
		c.WriteLen(4)
		c.WriteBulk(e.id)
		c.WriteBulk(e.consumer)
		c.WriteInt(e.millis)
		c.WriteInt(e.count)
	}
}

func parseBlock(cmd string, args []string, block *bool, timeout *time.Duration) error {
	if len(args) < 2 {
		return errors.New(errWrongNumber(cmd))
	}
	(*block) = true
	ms, err := strconv.Atoi(args[1])
	if err != nil {
		return errors.New(msgInvalidInt)
	}
	if ms < 0 {
		return errors.New("ERR timeout is negative")
	}
	(*timeout) = time.Millisecond * time.Duration(ms)
	return nil
}
