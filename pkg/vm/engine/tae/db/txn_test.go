// Copyright 2021 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package db

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pkgcatalog "github.com/matrixorigin/matrixone/pkg/catalog"
	"github.com/matrixorigin/matrixone/pkg/common/moerr"
	"github.com/matrixorigin/matrixone/pkg/container/types"
	"github.com/matrixorigin/matrixone/pkg/logutil"
	"github.com/matrixorigin/matrixone/pkg/objectio"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/catalog"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/common"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/containers"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/iface/handle"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/iface/txnif"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/model"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/options"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/testutils"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/txn/txnbase"
	"github.com/panjf2000/ants/v2"
	"github.com/stretchr/testify/assert"
)

var wareHouse *catalog.Schema
var district *catalog.Schema

var app1db = "app1"
var goods *catalog.Schema
var balance *catalog.Schema
var user *catalog.Schema
var deal *catalog.Schema
var repertory *catalog.Schema
var app1Conf *APP1Conf

var errNotEnoughRepertory = moerr.NewInternalErrorNoCtx("not enough repertory")

type APP1Conf struct {
	Users         int
	InitBalance   float64
	GoodKinds     int
	GoodRepertory int
}

type APP1Client struct {
	ID   uint64
	Name string
	Txn  txnif.AsyncTxn
	DB   handle.Database
	Rel  handle.Relation
}

type APP1Goods struct {
	ID    uint64
	Name  string
	Price float64
}

type APP1Repertory struct {
	ID      uint64
	GoodsID uint64
	Count   uint64
}

type APP1 struct {
	sync.RWMutex
	Clients []*APP1Client
	Goods   []*APP1Goods
	DBName  string
	Mgr     *txnbase.TxnManager
}

func init() {
	app1Conf = &APP1Conf{
		Users:         100,
		InitBalance:   1000000,
		GoodKinds:     2000,
		GoodRepertory: 100,
	}

	var err error
	wareHouse = catalog.NewEmptySchema("WAREHOUSE")
	wareHouse.BlockMaxRows = 40000
	wareHouse.SegmentMaxBlocks = 40
	_ = wareHouse.AppendPKCol("W_ID", types.T_uint8.ToType(), 0)
	_ = wareHouse.AppendCol("W_NAME", types.T_varchar.ToType())
	_ = wareHouse.AppendCol("W_STREET_1", types.T_varchar.ToType())
	_ = wareHouse.AppendCol("W_STREET_2", types.T_varchar.ToType())
	_ = wareHouse.AppendCol("W_CITY", types.T_varchar.ToType())
	_ = wareHouse.AppendCol("W_STATE", types.T_varchar.ToType())
	_ = wareHouse.AppendCol("W_ZIP", types.T_varchar.ToType())
	_ = wareHouse.AppendCol("W_TAX", types.T_float64.ToType())
	_ = wareHouse.AppendCol("W_YTD", types.T_float64.ToType())
	if err = wareHouse.Finalize(false); err != nil {
		panic(err)
	}

	district = catalog.NewEmptySchema("DISTRICT")
	district.BlockMaxRows = 40000
	district.SegmentMaxBlocks = 40
	_ = district.AppendPKCol("D_ID", types.T_int16.ToType(), 0)
	_ = district.AppendCol("D_W_ID", types.T_uint8.ToType())
	_ = district.AppendCol("D_NAME", types.T_varchar.ToType())
	_ = district.AppendCol("D_STREET_1", types.T_varchar.ToType())
	_ = district.AppendCol("D_STREET_2", types.T_varchar.ToType())
	_ = district.AppendCol("D_CITY", types.T_varchar.ToType())
	_ = district.AppendCol("D_STATE", types.T_varchar.ToType())
	_ = district.AppendCol("D_ZIP", types.T_varchar.ToType())
	_ = district.AppendCol("D_TAX", types.T_float64.ToType())
	_ = district.AppendCol("D_YTD", types.T_float64.ToType())
	_ = district.AppendCol("D_NEXT_O_ID", types.T_int64.ToType())
	if err = district.Finalize(false); err != nil {
		panic(err)
	}

	balance = catalog.NewEmptySchema("BALANCE")
	balance.BlockMaxRows = 40000
	balance.SegmentMaxBlocks = 40
	_ = balance.AppendPKCol("ID", types.T_uint64.ToType(), 0)
	_ = balance.AppendCol("BALANCE", types.T_float64.ToType())
	// balance.AppendCol("USERID", types.T_uint64.ToType())
	if err = balance.Finalize(false); err != nil {
		panic(err)
	}

	user = catalog.NewEmptySchema("USER")
	user.BlockMaxRows = 40000
	user.SegmentMaxBlocks = 40
	_ = user.AppendPKCol("ID", types.T_uint64.ToType(), 0)
	_ = user.AppendCol("NAME", types.T_varchar.ToType())
	_ = user.AppendCol("BIRTH", types.T_date.ToType())
	_ = user.AppendCol("ADDR", types.T_varchar.ToType())
	_ = user.AppendCol("BALANCEID", types.T_uint64.ToType())
	if err = user.Finalize(false); err != nil {
		panic(err)
	}

	goods = catalog.NewEmptySchema("GOODS")
	goods.BlockMaxRows = 40000
	goods.SegmentMaxBlocks = 40
	_ = goods.AppendPKCol("ID", types.T_uint64.ToType(), 0)
	_ = goods.AppendCol("NAME", types.T_varchar.ToType())
	_ = goods.AppendCol("PRICE", types.T_float64.ToType())
	_ = goods.AppendCol("DESC", types.T_varchar.ToType())
	if err = goods.Finalize(false); err != nil {
		panic(err)
	}

	repertory = catalog.NewEmptySchema("REPERTORY")
	repertory.BlockMaxRows = 40000
	repertory.SegmentMaxBlocks = 40
	_ = repertory.AppendPKCol("ID", types.T_uint64.ToType(), 0)
	_ = repertory.AppendCol("GOODID", types.T_uint64.ToType())
	_ = repertory.AppendCol("COUNT", types.T_uint64.ToType())
	if err = repertory.Finalize(false); err != nil {
		panic(err)
	}

	deal = catalog.NewEmptySchema("DEAL")
	deal.BlockMaxRows = 40000
	deal.SegmentMaxBlocks = 40
	_ = deal.AppendPKCol("ID", types.T_uint64.ToType(), 0)
	_ = deal.AppendCol("USERID", types.T_uint64.ToType())
	_ = deal.AppendCol("GOODID", types.T_uint64.ToType())
	_ = deal.AppendCol("QUANTITY", types.T_uint32.ToType())
	_ = deal.AppendCol("DEALTIME", types.T_datetime.ToType())
	if err = deal.Finalize(false); err != nil {
		panic(err)
	}
}

func NewApp1(mgr *txnbase.TxnManager, dbName string) *APP1 {
	return &APP1{
		Mgr:     mgr,
		DBName:  dbName,
		Clients: make([]*APP1Client, 0),
		Goods:   make([]*APP1Goods, 0),
	}
}

func NewAPP1UserClient(id uint64, name string) *APP1Client {
	return &APP1Client{
		ID:   id,
		Name: name,
	}
}

func (c *APP1Client) Clone() *APP1Client {
	return &APP1Client{
		ID:   c.ID,
		Name: c.Name,
	}
}

func (c *APP1Client) String() string {
	s := fmt.Sprintf("User:%d,%s", c.ID, c.Name)
	return s
}
func (c *APP1Client) Bind(db handle.Database, txn txnif.AsyncTxn) {
	if c.Txn != nil {
		panic("logic error")
	}
	c.Txn = txn
	c.DB = db
	c.Rel, _ = db.GetRelationByName(c.Name)
}

func (c *APP1Client) Unbind() {
	if c.Txn == nil {
		panic("logic error")
	}
	c.Txn = nil
	c.DB = nil
	c.Rel = nil
}

func (c *APP1Client) CheckBound() {
	if c.Txn == nil {
		panic("logic error")
	}
}

// TODO: rewrite
func (c *APP1Client) GetGoodRepetory(goodId uint64) (id *common.ID, offset uint32, count uint64, err error) {
	rel, _ := c.DB.GetRelationByName(repertory.Name)
	blockIt := rel.MakeBlockIt()
	var view *model.ColumnView
	found := false
	for blockIt.Valid() {
		blk := blockIt.GetBlock()
		view, err = blk.GetColumnDataByName(repertory.ColDefs[1].Name)
		if err != nil {
			return
		}
		defer view.Close()
		_ = view.GetData().Foreach(func(v any, _ bool, row int) (err error) {
			pk := v.(uint64)
			if pk != goodId {
				return
			}
			if view.DeleteMask != nil && view.DeleteMask.Contains(uint32(row)) {
				return
			}
			id = blk.Fingerprint()
			key := *objectio.NewRowid(&id.BlockID, uint32(row))
			cntv, _, err := rel.GetValueByPhyAddrKey(key, 2)
			if err != nil {
				return
			}
			found = true
			offset = uint32(row)
			count = cntv.(uint64)
			return moerr.NewInternalErrorNoCtx("stop iteration")
		}, nil)
		if found {
			return
		}
		blockIt.Next()
	}
	err = moerr.NewNotFoundNoCtx()
	return
}

// TODO: rewrite
func (c *APP1Client) GetGoodEntry(goodId uint64) (id *common.ID, offset uint32, entry *APP1Goods, err error) {
	filter := handle.NewEQFilter(goodId)
	goodRel, _ := c.DB.GetRelationByName(goods.Name)
	id, offset, err = goodRel.GetByFilter(filter)
	if err != nil {
		return
	}

	entry = new(APP1Goods)
	entry.ID = goodId
	price, _, _ := goodRel.GetValue(id, offset, 2)
	entry.Price = price.(float64)
	return
}

func (c *APP1Client) BuyGood(goodId uint64, count uint64) error {
	c.CheckBound()
	_, _, entry, err := c.GetGoodEntry(goodId)
	if err != nil {
		return err
	}
	_, _, left, err := c.GetGoodRepetory(entry.ID)
	if err != nil {
		return err
	}
	logutil.Debugf("%s, Count=%d", entry.String(), left)
	if count > left {
		logutil.Warnf("NotEnough Good %d: Repe %d, Requested %d", goodId, left, count)
		err = errNotEnoughRepertory
		return err
	}
	newLeft := left - count
	rel, _ := c.DB.GetRelationByName(repertory.Name)
	err = rel.UpdateByFilter(handle.NewEQFilter(entry.ID), uint16(2), newLeft, false)
	return err
}

func (g *APP1Goods) String() string {
	return fmt.Sprintf("GoodId:%d, GoodName:%s, GoodPrice:%f", g.ID, g.Name, g.Price)
}

func MockWarehouses(dbName string, num uint8, txn txnif.AsyncTxn) (err error) {
	db, err := txn.GetDatabase(dbName)
	if moerr.IsMoErrCode(err, moerr.ErrBadDB) {
		if db, err = txn.CreateDatabase(dbName, "", ""); err != nil {
			return
		}
	}
	rel, err := db.GetRelationByName(wareHouse.Name)
	if err == moerr.GetOkExpectedEOB() {
		if rel, err = db.CreateRelation(wareHouse); err != nil {
			return
		}
	}
	bat := catalog.MockBatch(wareHouse, int(num))
	defer bat.Close()
	err = rel.Append(bat)
	return
}

func GetWarehouseRelation(dbName string, txn txnif.AsyncTxn) (rel handle.Relation, err error) {
	db, _ := txn.GetDatabase(dbName)
	rel, err = db.GetRelationByName(wareHouse.Name)
	return
}

func GetOrCreateDatabase(name string, txn txnif.AsyncTxn) handle.Database {
	db, err := txn.GetDatabase(name)
	if moerr.IsMoErrCode(err, moerr.ErrBadDB) {
		if db, err = txn.CreateDatabase(name, "", ""); err != nil {
			panic(err)
		}
	}
	return db
}

func App1CreateTables(txn txnif.AsyncTxn) (db handle.Database, err error) {
	db = GetOrCreateDatabase(app1db, txn)
	if _, err = db.CreateRelation(user); err != nil {
		return
	}
	if _, err = db.CreateRelation(goods); err != nil {
		return
	}
	if _, err = db.CreateRelation(balance); err != nil {
		return
	}
	if _, err = db.CreateRelation(deal); err != nil {
		return
	}
	if _, err = db.CreateRelation(repertory); err != nil {
		return
	}
	return
}

func (app1 *APP1) GetClient() *APP1Client {
	idx := rand.Intn(len(app1.Clients))
	return app1.Clients[idx].Clone()
}

func (app1 *APP1) GetGoods() *APP1Goods {
	idx := rand.Intn(len(app1.Goods))
	return app1.Goods[idx]
}

func (app1 *APP1) Init(factor int) {
	txn, _ := app1.Mgr.StartTxn(nil)
	defer func() {
		err := txn.Commit()
		if err != nil {
			panic(err)
		}
	}()
	db, err := App1CreateTables(txn)
	if err != nil {
		panic(err)
	}
	conf := *app1Conf
	conf.GoodKinds *= factor
	conf.GoodRepertory *= factor
	conf.Users *= factor
	balanceRel, err := db.GetRelationByName(balance.Name)
	if err != nil {
		panic(err)
	}
	balanceData := catalog.MockBatch(balance, int(conf.Users))
	defer balanceData.Close()
	if err = balanceRel.Append(balanceData); err != nil {
		panic(err)
	}

	userRel, err := db.GetRelationByName(user.Name)
	if err != nil {
		panic(err)
	}
	provider := containers.NewMockDataProvider()
	provider.AddColumnProvider(4, balanceData.Vecs[0])
	userData := containers.MockBatchWithAttrs(user.Types(), user.Attrs(), conf.Users, user.GetSingleSortKeyIdx(), provider)
	defer userData.Close()

	for i := 0; i < conf.Users; i++ {
		uid := userData.Vecs[0].Get(i)
		uname := userData.Vecs[1].Get(i)
		client := NewAPP1UserClient(uid.(uint64), string(uname.([]byte)))
		app1.Clients = append(app1.Clients, client)
		// logutil.Info(client.String())
	}

	if err = userRel.Append(userData); err != nil {
		panic(err)
	}
	price := containers.MakeVector(goods.ColDefs[2].Type)
	defer price.Close()
	for i := 0; i < conf.GoodKinds; i++ {
		goodPrice := float64(rand.Intn(1000)+20) / float64(rand.Intn(10)+1) / float64(20)
		price.Append(goodPrice, false)
	}
	goodsRel, err := db.GetRelationByName(goods.Name)
	if err != nil {
		panic(err)
	}
	provider.Reset()
	provider.AddColumnProvider(2, price)
	goodsData := containers.MockBatchWithAttrs(goods.Types(), goods.Attrs(), conf.GoodKinds, goods.GetSingleSortKeyIdx(), provider)
	defer goodsData.Close()
	if err = goodsRel.Append(goodsData); err != nil {
		panic(err)
	}

	goodIds := goodsData.Vecs[0]
	count := containers.MakeVector(repertory.ColDefs[2].Type)
	defer count.Close()
	for i := 0; i < conf.GoodKinds; i++ {
		goodCount := rand.Intn(1000) + 100
		count.Append(uint64(goodCount), false)
		goodsId := goodsData.Vecs[0].Get(i)
		goodsName := goodsData.Vecs[1].Get(i)
		goods := new(APP1Goods)
		goods.ID = goodsId.(uint64)
		goods.Name = string(goodsName.([]byte))
		app1.Goods = append(app1.Goods, goods)
	}
	provider.Reset()
	provider.AddColumnProvider(1, goodIds)
	provider.AddColumnProvider(2, count)
	repertoryData := containers.MockBatchWithAttrs(repertory.Types(), repertory.Attrs(), int(conf.GoodKinds), repertory.GetSingleSortKeyIdx(), provider)
	defer repertoryData.Close()
	repertoryRel, err := db.GetRelationByName(repertory.Name)
	if err != nil {
		panic(err)
	}
	if err = repertoryRel.Append(repertoryData); err != nil {
		panic(err)
	}
}

func TestApp1(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	option := new(options.Options)
	option.CacheCfg = new(options.CacheCfg)
	option.CacheCfg.IndexCapacity = common.G
	db := initDB(t, option)
	defer db.Close()
	mgr := db.TxnMgr
	c := db.Opts.Catalog

	app1 := NewApp1(mgr, "app1")
	app1.Init(1)

	p, _ := ants.NewPool(100)
	defer p.Release()

	var wg sync.WaitGroup
	buyTxn := func() {
		defer wg.Done()
		txn, _ := mgr.StartTxn(nil)
		client := app1.GetClient()
		db, _ := txn.GetDatabase(app1.DBName)
		client.Bind(db, txn)
		goods := app1.GetGoods()
		err := client.BuyGood(goods.ID, uint64(rand.Intn(2)+10))
		if err != nil {
			// t.Log(err)
			err := txn.Rollback()
			assert.Nil(t, err)
		} else {
			err := txn.Commit()
			assert.Nil(t, err)
		}
		if txn.GetTxnState(true) == txnif.TxnStateRollbacked {
			t.Log(txn.String())
		}
	}
	for i := 0; i < 500; i++ {
		wg.Add(1)
		err := p.Submit(buyTxn)
		assert.Nil(t, err)
	}
	wg.Wait()
	t.Log(c.SimplePPString(common.PPL1))
	{
		// txn := mgr.StartTxn(nil)
		// db, _ := txn.GetDatabase(app1.DBName)
		// rel, _ := db.GetRelationByName(repertory.Name)
		// t.Log(rel.SimplePPString(common.PPL1))
	}
}

func TestWarehouse(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	db := initDB(t, nil)
	defer db.Close()

	txn, _ := db.StartTxn(nil)
	err := MockWarehouses("test", 20, txn)
	assert.Nil(t, err)
	assert.Nil(t, txn.Commit())
	t.Log(db.Opts.Catalog.SimplePPString(common.PPL1))

	{
		txn, _ = db.StartTxn(nil)
		rel, err := GetWarehouseRelation("test", txn)
		assert.Nil(t, err)
		it := rel.MakeBlockIt()
		blk := it.GetBlock()
		view, _ := blk.GetColumnDataById(1)
		t.Log(view.GetData().String())
		defer view.Close()
		checkAllColRowsByScan(t, rel, 20, false)
		_ = txn.Commit()
	}
}

func TestTxn7(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	tae := initDB(t, nil)
	defer tae.Close()
	schema := catalog.MockSchemaAll(13, 12)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2

	bat := catalog.MockBatch(schema, 20)
	defer bat.Close()

	txn, _ := tae.StartTxn(nil)
	db, err := txn.CreateDatabase("db", "", "")
	assert.NoError(t, err)
	_, err = db.CreateRelation(schema)
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit())

	txn, _ = tae.StartTxn(nil)
	db, _ = txn.GetDatabase("db")
	rel, _ := db.GetRelationByName(schema.Name)
	err = rel.Append(bat)
	assert.NoError(t, err)
	{
		txn, _ := tae.StartTxn(nil)
		db, _ := txn.GetDatabase("db")
		rel, _ := db.GetRelationByName(schema.Name)
		err := rel.Append(bat)
		assert.NoError(t, err)
		assert.NoError(t, txn.Commit())
	}
	err = txn.Commit()
	t.Log(err)
	assert.Error(t, err)
	t.Log(txn.String())
}

func TestTxn8(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	tae := initDB(t, nil)
	schema := catalog.MockSchemaAll(13, 2)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2

	bat := catalog.MockBatch(schema, int(schema.BlockMaxRows*10))
	defer bat.Close()
	bats := bat.Split(2)

	txn, _ := tae.StartTxn(nil)
	db, _ := txn.GetDatabase(pkgcatalog.MO_CATALOG)
	rel, _ := db.CreateRelation(schema)
	err := rel.Append(bats[0])
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit())

	txn, _ = tae.StartTxn(nil)
	db, _ = txn.GetDatabase(pkgcatalog.MO_CATALOG)
	rel, _ = db.GetRelationByName(schema.Name)
	err = rel.Append(bats[1])
	assert.NoError(t, err)
	pkv := bats[0].Vecs[schema.GetSingleSortKeyIdx()].Get(2)
	filter := handle.NewEQFilter(pkv)
	err = rel.UpdateByFilter(filter, 3, int64(9999), false)
	assert.NoError(t, err)

	pkv = bats[0].Vecs[schema.GetSingleSortKeyIdx()].Get(3)
	filter = handle.NewEQFilter(pkv)
	id, row, err := rel.GetByFilter(filter)
	assert.NoError(t, err)
	err = rel.RangeDelete(id, row, row, handle.DT_Normal)
	assert.NoError(t, err)

	tae.Close()

	_, err = tae.StartTxn(nil)
	assert.Error(t, err)

	err = txn.Commit()
	t.Log(err)
}

// Test wait committing
func TestTxn9(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	tae := initDB(t, nil)
	defer tae.Close()

	schema := catalog.MockSchemaAll(13, 12)
	schema.BlockMaxRows = 20
	schema.SegmentMaxBlocks = 4
	expectRows := schema.BlockMaxRows * 5 / 2
	bat := catalog.MockBatch(schema, int(expectRows))
	defer bat.Close()
	bats := bat.Split(5)

	txn, _ := tae.StartTxn(nil)
	db, _ := txn.CreateDatabase("db", "", "")
	_, _ = db.CreateRelation(schema)
	assert.NoError(t, txn.Commit())

	var wg sync.WaitGroup

	var val atomic.Uint32

	scanNames := func() {
		defer wg.Done()
		txn, _ := tae.StartTxn(nil)
		db, _ := txn.GetDatabase("db")
		it := db.MakeRelationIt()
		cnt := 0
		for it.Valid() {
			cnt++
			it.Next()
		}
		val.Store(2)
		// Use max commit ts as start ts
		// 2nd relation is not visible
		assert.Equal(t, 1, cnt)
		assert.NoError(t, txn.Commit())
	}

	scanCol := func() {
		defer wg.Done()
		txn, _ := tae.StartTxn(nil)
		db, _ := txn.GetDatabase("db")
		rel, _ := db.GetRelationByName(schema.Name)
		rows := 0
		it := rel.MakeBlockIt()
		for it.Valid() {
			blk := it.GetBlock()
			view, err := blk.GetColumnDataById(2)
			assert.NoError(t, err)
			defer view.Close()
			t.Log(view.GetData().String())
			rows += blk.Rows()
			it.Next()
		}
		val.Store(2)
		// assert.Equal(t, int(expectRows/5*2), rows)
		assert.NoError(t, txn.Commit())
	}

	txn, _ = tae.StartTxn(nil)
	db, _ = txn.GetDatabase("db")
	txn.SetApplyCommitFn(func(txn txnif.AsyncTxn) error {
		wg.Add(1)
		go scanNames()
		time.Sleep(time.Millisecond * 10)
		val.Store(1)
		store := txn.GetStore()
		return store.ApplyCommit()
	})
	schema2 := catalog.MockSchemaAll(13, 12)
	_, _ = db.CreateRelation(schema2)
	rel, _ := db.GetRelationByName(schema.Name)
	err := rel.Append(bats[0])
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit())
	wg.Wait()
	// Use max commit ts as start ts
	// When reading snapshot, it's not necessary to wait commit.
	assert.Equal(t, uint32(1), val.Load())

	apply := func(_ txnif.AsyncTxn) error {
		wg.Add(1)
		go scanCol()
		time.Sleep(time.Millisecond * 10)
		val.Store(1)
		store := txn.GetStore()
		return store.ApplyCommit()
	}

	txn, _ = tae.StartTxn(nil)
	db, _ = txn.GetDatabase("db")
	txn.SetApplyCommitFn(apply)
	rel, _ = db.GetRelationByName(schema.Name)
	err = rel.Append(bats[1])
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit())
	wg.Wait()

	txn, _ = tae.StartTxn(nil)
	db, _ = txn.GetDatabase("db")
	txn.SetApplyCommitFn(apply)
	rel, _ = db.GetRelationByName(schema.Name)
	v := bats[0].Vecs[schema.GetSingleSortKeyIdx()].Get(2)
	filter := handle.NewEQFilter(v)
	id, row, err := rel.GetByFilter(filter)
	assert.NoError(t, err)
	err = rel.RangeDelete(id, row, row, handle.DT_Normal)
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit())
	wg.Wait()

	txn, _ = tae.StartTxn(nil)
	db, _ = txn.GetDatabase("db")
	txn.SetApplyCommitFn(apply)
	rel, _ = db.GetRelationByName(schema.Name)
	v = bats[0].Vecs[schema.GetSingleSortKeyIdx()].Get(3)
	filter = handle.NewEQFilter(v)
	err = rel.UpdateByFilter(filter, 2, int32(9999), false)
	assert.NoError(t, err)
	assert.NoError(t, txn.Commit())
	wg.Wait()
}

// func TestTxn10(t *testing.T) {
// 	opts := config.WithLongScanAndCKPOpts(nil)
// 	tae := newTestEngine(t, opts)
// 	defer tae.Close()
// 	schema := catalog.MockSchemaAll(18, 2)
// 	tae.bindSchema(schema)
// 	bat := catalog.MockBatch(schema, 5)
// 	defer bat.Close()
// 	tae.createRelAndAppend(bat.Window(0, 2), true)

// 	txn1, rel1 := tae.getRelation()
// 	blk := getOneBlock(rel1)
// 	view, err := blk.GetColumnDataById(2, nil, nil)
// 	assert.NoError(t, err)
// 	defer view.Close()
// 	t.Log(view.String())
// 	err = rel1.Append(bat.Window(2, 1))
// 	assert.NoError(t, err)
// 	blk = getOneBlock(rel1)
// 	view, err = blk.GetColumnDataById(2, nil, nil)
// 	assert.NoError(t, err)
// 	defer view.Close()
// 	t.Log(view.String())
// 	{
// 		txn, rel := tae.getRelation()
// 		err := rel.Append(bat.Window(2, 1))
// 		assert.NoError(t, err)
// 		assert.NoError(t, txn.Commit())
// 		txn, rel = tae.getRelation()
// 		blk := getOneBlock(rel)
// 		view, err := blk.GetColumnDataById(2, nil, nil)
// 		assert.NoError(t, err)
// 		defer view.Close()
// 		t.Log(view.String())
// 		assert.NoError(t, txn.Commit())
// 	}

// 	// filter := handle.NewEQFilter(int32(99))
// 	// err = rel1.DeleteByFilter(filter)
// 	// assert.NoError(t, err)
// 	win := bat.CloneWindow(2, 1)
// 	win.Vecs[2].Update(0, int32(99))
// 	err = rel1.Append(win)
// 	{
// 		// filter := handle.NewEQFilter(int32(99))
// 		// txn, rel := tae.getRelation()
// 		// err = rel1.UpdateByFilter(filter, 2, int32(88))
// 		// assert.NoError(t, err)
// 		// assert.NoError(t, txn.Commit())
// 	}
// 	return
// 	assert.NoError(t, txn1.Commit())
// }

// func TestTxn11(t *testing.T) {
// 	opts := config.WithLongScanAndCKPOpts(nil)
// 	tae := newTestEngine(t, opts)
// 	defer tae.Close()
// 	schema := catalog.MockSchema(2, 0)
// 	tae.bindSchema(schema)
// 	bat1 := catalog.MockBatch(schema, 0)
// 	defer bat1.Close()
// 	bat1.Vecs[0].AppendMany(int32(1), int32(2))
// 	bat1.Vecs[1].AppendMany(int32(1), int32(2))
// 	bat2 := catalog.MockBatch(schema, 0)
// 	defer bat2.Close()
// 	bat2.Vecs[0].Append(int32(3))
// 	bat2.Vecs[0].Append(int32(4))
// 	bat2.Vecs[1].Append(int32(1))
// 	bat2.Vecs[1].Append(int32(2))

// 	tae.createRelAndAppend(bat1, true)

// 	buffer := new(bytes.Buffer)

// 	txn, rel := tae.getRelation()
// 	blk := getOneBlock(rel)
// 	view, err := blk.GetColumnDataById(0, nil, buffer)
// 	assert.NoError(t, err)
// 	defer view.Close()
// 	view, err = blk.GetColumnDataById(1, nil, buffer)
// 	assert.NoError(t, err)
// 	defer view.Close()

// 	err = rel.Append(bat2)
// 	assert.NoError(t, err)
// 	it := rel.MakeBlockIt()
// 	for it.Valid() {
// 		blk = it.GetBlock()
// 		t.Log(blk.Fingerprint().String())
// 		view, err = blk.GetColumnDataById(0, nil, buffer)
// 		assert.NoError(t, err)
// 		defer view.Close()
// 		t.Log(view.String())
// 		view, err = blk.GetColumnDataById(1, nil, buffer)
// 		assert.NoError(t, err)
// 		defer view.Close()
// 		t.Log(view.String())
// 		it.Next()
// 	}
// 	filter := handle.NewEQFilter(int32(1))
// 	err = rel.DeleteByFilter(filter)
// 	checkAllColRowsByScan(t, rel, 3, true)
// 	assert.NoError(t, err)
// 	{
// 		txn, rel := tae.getRelation()
// 		it := rel.MakeBlockIt()
// 		for it.Valid() {
// 			blk := it.GetBlock()
// 			view, err := blk.GetColumnDataById(0, nil, buffer)
// 			assert.NoError(t, err)
// 			defer view.Close()
// 			t.Log(view.String())
// 			it.Next()
// 		}

// 		assert.NoError(t, txn.Commit())
// 	}

// 	txn.Commit()
// }
