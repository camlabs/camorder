package orderservice

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	// "time"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"math/rand"
	"strings"
	"sync"

	"github.com/dynamicgo/config"
	"github.com/dynamicgo/slf4go"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis"
	"github.com/go-xorm/xorm"

	"github.com/camlabs/camdb"
	"github.com/camlabs/camorder/claim"
	"github.com/camlabs/camorder/model"

	"github.com/Jeffail/gabs"
	_ "github.com/go-sql-driver/mysql"

	"github.com/camlabs/camgo/rpc"
)

// HTTPServer .
type HTTPServer struct {
	mutex  sync.Mutex
	engine *gin.Engine
	slf4go.Logger
	laddr        string
	db           *xorm.Engine
	conf         *config.Config
	client       *rpc.Client
	redisclient  *redis.Client
	syncChan     chan *syncAddress
	syncFlag     map[string]string
	syncTimes    int
	syncDuration time.Duration
}

// NewHTTPServer .
func NewHTTPServer(cnf *config.Config) (*HTTPServer, error) {

	if !cnf.GetBool("order.debug", true) {
		gin.SetMode(gin.ReleaseMode)
	}

	// f, _ := os.Create("gin.log")
	// gin.DefaultWriter = io.MultiWriter(f)

	engine := gin.New()

	engine.Use(gin.Recovery())

	username := cnf.GetString("order.camdb.username", "xxx")
	password := cnf.GetString("order.camdb.password", "xxx")
	port := cnf.GetString("order.camdb.port", "6543")
	host := cnf.GetString("order.camdb.host", "localhost")
	scheme := cnf.GetString("order.camdb.schema", "postgres")

	db, err := xorm.NewEngine(
		"mysql",
		fmt.Sprintf(
			"%v:%v@(%v:%v)/%v?charset=utf8",
			username, password, host, port, scheme,
		),
	)

	db.SetConnMaxLifetime(time.Second * 60)

	db.SetMaxIdleConns(20)

	db.SetMaxOpenConns(20)

	//直接执行sql，并赋值到对应的表中
	// engine.Sql("select * from table").Find(&beans)
	//执行sql命令
	//sql = "update `userinfo` set username=? where id=?"
	//res, err := engine.Exec(sql, "xiaolun", 1)

	db.ShowSQL(true)

	if err != nil {
		return nil, err
	}

	client := redis.NewClient(&redis.Options{
		Addr:     cnf.GetString("order.redis.address", "127.0.0.1:6379"),
		Password: cnf.GetString("order.redis.password", "123456"), // no password set
		DB:       int(cnf.GetInt64("order.redis.db", 1)),          // use default DB
	})

	service := &HTTPServer{
		engine:       engine,
		Logger:       slf4go.Get("cam-order-service"),
		laddr:        cnf.GetString("order.laddr", ":8000"),
		db:           db,
		conf:         cnf,
		client:       rpc.NewClient(cnf.GetString("order.camrpc", "http://localhost:16332")),
		redisclient:  client,
		syncChan:     make(chan *syncAddress, cnf.GetInt64("order.sync_chan_length", 1024)),
		syncFlag:     make(map[string]string),
		syncTimes:    int(cnf.GetInt64("order.sync_times", 20)),
		syncDuration: time.Second * cnf.GetDuration("order.sync_duration", 4),
	}

	service.makeRouters()

	return service, nil
}

// Run run http service
func (service *HTTPServer) Run() error {

	go service.syncCached()

	return service.engine.Run(service.laddr)
}

func (server *HTTPServer) syncCached() {

	for address := range server.syncChan {

		server.DebugF("sync address claimed utxos %s", address)

		startTime := time.Now()

		unclaimed, err := server.doGetClaim(address.Address)

		//计算读取可提取gas信息所使用的时间
		claimTimes := time.Now().Sub(startTime)

		if err != nil {
			server.ErrorF("sync claim for address %s err, %s", address, err)
			continue
		}

		server.DebugF("[doGetClaim] claim %s spent times %s", address.Address, claimTimes)

		data, err := json.Marshal(unclaimed)

		if err != nil {
			server.ErrorF("sync claim for address %s err, %s", address, err)
			continue
		}

		err = server.redisclient.Set(address.Address, data, time.Hour*24).Err()

		if err != nil {
			server.ErrorF("cached claim for address %s err, %s", address, err)
			continue
		}

		server.DebugF(" sync address claimed utxos %s -- success", address)

		//地址的查询次数减1
		address.Times--

		if address.Times > 0 {

			requeue := address

			//初始值4s
			syncDuration := server.syncDuration

			//如果查询时间大于20s，则10分钟查一次
			if claimTimes > 20*time.Second {
				syncDuration = time.Minute * 10
			}

			//获取的间隔查询小于之前的间隔查询，则使用之前的间隔查询
			if syncDuration < server.syncDuration {
				syncDuration = server.syncDuration
			}

			//设置下次间隔执行函数
			//推入地址到队列中
			time.AfterFunc(syncDuration, func() {
				server.DebugF("requeue sync address %s", requeue)

				server.syncChan <- requeue

				server.DebugF("requeue sync address %s -- success", requeue)
			})

		} else {
			server.DebugF("delete sync address %s", address)
			server.removeAddress(address.Address)
			server.DebugF("delete sync address %s -- success", address)
		}
	}

}

func (server *HTTPServer) removeAddress(address string) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	delete(server.syncFlag, address)

}

func (server *HTTPServer) markAddress(address string) bool {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	flag := false

	if _, ok := server.syncFlag[address]; !ok {
		server.syncFlag[address] = address

		flag = true

		server.DebugF("queued claim task: %s", address)
	}

	return flag
}

func (server *HTTPServer) getCachedClaim(address string) (unclaimed *rpc.Unclaimed, ok bool) {

	server.DebugF("get claim: %s", address)

	flag := server.markAddress(address)

	if flag {
		server.syncChan <- &syncAddress{
			Address: address,
			Times:   server.syncTimes,
		}
	}

	val, err := server.redisclient.Get(address).Result()

	if err == redis.Nil {
		return nil, false
	}

	if err != nil {
		server.DebugF("get cached claim for address %s err , %s", address, err)
		return nil, false
	}

	err = json.Unmarshal([]byte(val), &unclaimed)

	if err != nil {
		server.DebugF("get cachde claim for address %s err , %s", address, err)
		return nil, false
	}

	ok = true

	server.DebugF("get cached claim for address %s -- success", address)

	return
}

func (service *HTTPServer) makeRouters() {

	service.engine.GET("/", func(ctx *gin.Context) {
		service.apiLog(ctx, nil)
		service.success(ctx, "success", "api is start success")
	})

	service.engine.POST("/addaddress", func(ctx *gin.Context) {

		var walletInfo *HttpAddWallet

		err := ctx.BindJSON(&walletInfo)

		if err != nil {
			service.error(ctx, err.Error())
			return
		}

		//日志输出
		service.apiLog(ctx, walletInfo)

		if walletInfo.Address == "" {
			service.error(ctx, "address is null")
			return
		}

		//创建钱包
		if err := service.createWallet(walletInfo.Address); err != nil {

			service.ErrorF("create wallet error :%s", err)
			//忽略key重复的错误
			errMsg := err.Error()

			println(errMsg)
			//如果是索引引发的错误则忽略
			if strings.Contains(errMsg, "address") {

				service.DebugF("Duplicate entry for key error, ignore ")

				// go service.updateWalletOrder(walletInfo.Address)

				service.success(ctx, "success", "Duplicate entry for key error,but add address success")

			} else {
				service.error(ctx, err.Error())

			}

			return
		}

		// go service.updateWalletOrder(walletInfo.Address)

		service.success(ctx, "success", "add address success")

	})

	service.engine.POST("/addorder", func(ctx *gin.Context) {

		var order *camdb.Order

		if err := ctx.ShouldBindJSON(&order); err != nil {
			service.ErrorF("parse order error :%s", err)

			service.error(ctx, err.Error())
			return
		}

		//日志输出
		service.apiLog(ctx, order)

		//用户添加的订单，都为待确认状态
		order.Status = 1

		id, err := service.createOrder(order)
		if err != nil {
			service.ErrorF("create order error :%s", err)

			service.error(ctx, err.Error())
			return
		}

		//30分钟后检查订单的状态，如果block还为-1，则认为失败
		go service.checkOrderStatus(order.TX)

		service.success(ctx, "success", gin.H{"id": id})
	})

	service.engine.POST("/utxolist", func(ctx *gin.Context) {

		var httpUtxoList *HttpUtxoList

		if err := ctx.ShouldBindJSON(&httpUtxoList); err != nil {
			service.ErrorF("parse parameters error :%s", err)

			service.error(ctx, err.Error())
			return
		}

		//日志输出
		service.apiLog(ctx, httpUtxoList)

		if utxoList, err := service.getUtxoList(httpUtxoList.Asset, httpUtxoList.Address); err != nil {
			service.ErrorF("get utxolist error :%s", err)
			// ctx.JSON(http.StatusOK, gin.H{"error":err.Error()})
			service.error(ctx, err.Error())

		} else {
			//计算余额
			var totalBalance float64
			for _, utxoUnspent := range utxoList {

				tempValue, err := strconv.ParseFloat(utxoUnspent.Value, 64)
				if err == nil {
					totalBalance += tempValue
				}

			}

			//组装成camgo可识别的结构
			// ctx.JSON(http.StatusOK,gin.H{"balance":totalBalance,"unspent":utxoList} )
			service.success(ctx, "success", gin.H{"balance": totalBalance, "unspent": utxoList})
		}
	})

	service.engine.POST("/getclaim", func(ctx *gin.Context) {

		var walletInfo *HttpAddWallet

		err := ctx.BindJSON(&walletInfo)

		if err != nil {
			service.error(ctx, err.Error())
			return
		}

		//日志输出
		service.apiLog(ctx, walletInfo)

		if walletInfo.Address == "" {
			service.error(ctx, "address is null")
			return
		}

		// claimMsg,err := service.doGetClaim(walletInfo.Address)

		// if err != nil{
		// 	service.error(ctx,err.Error())
		// 	return
		// }

		unclaimed, ok := service.getCachedClaim(walletInfo.Address)

		if !ok {
			utxoEmpty := make([]*rpc.UTXO, 0)
			unclaimed = &rpc.Unclaimed{
				Unavailable: "0",
				Available:   "0",
				Claims:      utxoEmpty,
			}
		}

		service.success(ctx, "success", unclaimed)

	})

	service.engine.POST("/getaccountbalance", func(ctx *gin.Context) {
		var httpUtxoList *HttpUtxoList

		if err := ctx.ShouldBindJSON(&httpUtxoList); err != nil {
			service.ErrorF("parse parameters error :%s", err)

			service.error(ctx, err.Error())
			return
		}

		//日志输出
		service.apiLog(ctx, httpUtxoList)

		if utxoList, err := service.getUtxoList(httpUtxoList.Asset, httpUtxoList.Address); err != nil {
			service.ErrorF("get utxolist error :%s", err)
			// ctx.JSON(http.StatusOK, gin.H{"error":err.Error()})
			service.error(ctx, err.Error())

		} else {
			//计算余额
			var totalBalance float64
			for _, utxoUnspent := range utxoList {

				tempValue, err := strconv.ParseFloat(utxoUnspent.Value, 64)
				if err == nil {
					totalBalance += tempValue
				}

			}

			//组装成camgo可识别的结构
			// ctx.JSON(http.StatusOK,gin.H{"balance":totalBalance,"unspent":utxoList} )
			service.success(ctx, "success", gin.H{"balance": totalBalance})
		}
	})

	service.engine.POST("/getcac20balance", func(ctx *gin.Context) {

		var httpUtxoList *HttpUtxoList

		if err := ctx.ShouldBindJSON(&httpUtxoList); err != nil {
			service.ErrorF("parse parameters error :%s", err)

			service.error(ctx, err.Error())
			return
		}

		//日志输出
		service.apiLog(ctx, httpUtxoList)

		//进行rpc请求
		balance, err := service.client.Cac20BalanceOf(httpUtxoList.Asset, httpUtxoList.Address)

		if err != nil {
			service.error(ctx, err.Error())
			return
		}

		service.success(ctx, "success", gin.H{"balance": balance})

	})

	service.engine.POST("/getorderbytx", func(ctx *gin.Context) {

		var searchOrderByTx *SearchOrderByTx

		if err := ctx.ShouldBindJSON(&searchOrderByTx); err != nil {
			service.ErrorF("parse parameters error :%s", err)

			service.error(ctx, err.Error())
			return
		}

		//日志输出
		service.apiLog(ctx, searchOrderByTx)

		if orders, err := service.getOrder(searchOrderByTx.TX); err != nil {
			service.ErrorF("get orders error :%s", err)
			// ctx.JSON(http.StatusOK, gin.H{"error": err.Error()})
			service.error(ctx, err.Error())
		} else {

			//增加
			// ctx.JSON(http.StatusOK, orders)
			service.success(ctx, "success", orders)
		}
	})

	//根据tx哈希值，以及行id，返回订单列表
	service.engine.POST("/getorderbytxandid", func(ctx *gin.Context) {

		var searchTx *SearchOrderByTxAndId

		err := ctx.BindJSON(&searchTx)

		if err != nil {
			service.error(ctx, err.Error())
			return
		}

		//日志输出
		service.apiLog(ctx, searchTx)

		// println(searchTx.TX)
		orderList, err := service.getOrderByTxAndId(searchTx.TX, searchTx.Id)

		if err != nil {
			service.error(ctx, err.Error())
			return
		}

		service.success(ctx, "success", orderList)

	})

	//根据地址列表，返回所有的订单记录，可以分页
	service.engine.POST("/getorderbyaddress", func(ctx *gin.Context) {

		var searchTx *SearchOrderPageAddress

		err := ctx.BindJSON(&searchTx)

		if err != nil {
			service.error(ctx, err.Error())
			return
		}

		//日志输出
		service.apiLog(ctx, searchTx)

		// if len(searchTx.AddressList) == 0{
		// 	service.error(ctx,"address count equal 0")
		// 	return
		// }

		orderList, err := service.getOrderByAddressList(searchTx.Offset, searchTx.Size, searchTx.Address)
		if err != nil {
			service.error(ctx, err.Error())
			return
		}

		service.success(ctx, "success", orderList)

	})

	//根据地址列表，合约地址，返回所有的订单记录，可以分页
	service.engine.POST("/getorderbyaddressasset", func(ctx *gin.Context) {

		var searchTx *SearchOrderPageAddressAsset

		err := ctx.BindJSON(&searchTx)

		if err != nil {
			service.error(ctx, err.Error())
			return
		}

		//日志输出
		service.apiLog(ctx, searchTx)

		// if len(searchTx.AddressList) == 0{
		// 	service.error(ctx,"address count equal 0")
		// 	return
		// }

		orderList, err := service.getOrderByAddressAssetList(searchTx.Offset, searchTx.Size, searchTx.Address, searchTx.Asset)
		if err != nil {
			service.error(ctx, err.Error())
			return
		}

		service.success(ctx, "success", orderList)

	})

}

func parseInt(ctx *gin.Context, name string) (int, error) {
	result, err := strconv.ParseInt(ctx.Param(name), 10, 32)

	return int(result), err
}

func (service *HTTPServer) createWallet(address string) error {

	wallet := &camdb.Wallet{
		Address:    address,
		CreateTime: time.Now().Unix(),
	}

	_, err := service.db.Insert(wallet)

	return err
}

func (service *HTTPServer) updateWalletOrder(address string) {
	//创建地址对应的历史订单，需要检索所有地址所涉及的交易，并创建订单
	//查找交易是否存在
	neoTxs := make([]*camdb.Tx, 0)

	err := service.db.Where(" `from` = ? or `to` = ? ", address, address).Find(&neoTxs)
	if err != nil {
		service.ErrorF("find tx error :%s", err.Error())
		// ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	//根据查询的交易创建订单
	//逐个添加，忽略key错误
	var orders []*camdb.Order

	if len(neoTxs) == 0 {
		println("find tx count 0")
	}

	for _, tx := range neoTxs {

		//查看tx_id是否存在
		//如果存在则不添加
		//错误则忽略
		//只打印到日志中
		//查找订单是否存在
		camOrder2 := make([]*camdb.Order, 0)

		err := service.db.Where(" t_x = ? and `from` = ? and `to` = ?", tx.TX, tx.From, tx.To).Find(&camOrder2)

		// etl.DebugF("find tx %s",txid)
		//查询错误则返回错误
		//重新读取区块，并填入交易
		if err != nil {
			// return err
			//有错误则继续，不退出
			//但要输出错误日志
			println("find order error")
			continue
		}

		//不存在记录则返回，则添加
		if len(camOrder2) == 0 {
			order := new(camdb.Order)

			order.Asset = tx.Asset
			order.From = tx.From
			order.To = tx.To
			order.TX = tx.TX
			order.Value = tx.Value
			order.CreateTime = tx.CreateTime
			order.ConfirmTime = tx.CreateTime
			order.Block = int64(tx.Block)
			order.Status = 2
			orders = append(orders, order)
		} else {
			println("====================================")
			//存在记录则不处理
			fmt.Printf("exists order order tx %s", tx.TX)
		}
	}

	if len(orders) > 0 {

		_, err = service.db.Insert(&orders)

		if err != nil {
			// return err
			errMsg := err.Error()
			//如果是索引引发的错误则忽略
			if strings.Contains(errMsg, "t_x_from_to_asset") {
				// etl.DebugF("Duplicate entry for key error, ignore ")
				println("add wallet order Duplicate entry for key error, ignore ")
			} else {
				service.ErrorF("add wallet order error :%s", err)
				// ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
		}
	}

	println("add order success")

	return
}

func (service *HTTPServer) deleteWallet(userid int64, address string) error {

	wallet := &camdb.Wallet{
		Address: address,
	}

	_, err := service.db.Delete(wallet)
	return err
}

//获取未花资产
func (service *HTTPServer) getUtxoList(assetid string, address string) ([]*UTXOJson, error) {

	service.DebugF("getUtxoList address %s ", address)

	utxos := make([]*UTXO, 0)

	err := service.db.
		Where(`asset = ? and address = ? and spent_block = -1`, assetid, address).
		Asc("value").
		Find(&utxos)

	if err != nil {
		return make([]*UTXOJson, 0), err
	}

	//组装utxojson
	var utxoJsonAr []*UTXOJson

	for _, utxo := range utxos {
		vout := new(Vout)
		vout.Address = utxo.Address
		vout.Asset = utxo.Asset
		vout.Value = utxo.Value
		vout.N = utxo.N
		utxoJson := new(UTXOJson)
		utxoJson.Vout = *vout
		utxoJson.TransactionID = utxo.TX
		utxoJson.CreateTime = string(utxo.CreateTime)
		utxoJson.SpentTime = string(utxo.SpentTime)
		utxoJson.Block = utxo.CreateBlock
		utxoJson.SpentBlock = utxo.SpentBlock
		utxoJson.Value = utxo.Value
		utxoJson.Gas = "0"

		utxoJsonAr = append(utxoJsonAr, utxoJson)

	}

	return utxoJsonAr, nil
}

//返回的
type UTXOJson struct {
	TransactionID string `json:"txid"`
	Vout          Vout   `json:"Vout"`
	CreateTime    string `json:"CreateTime"`
	SpentTime     string `json:"SpentTime"`
	Block         int64  `json:"Block"`
	SpentBlock    int64  `json:"SpentBlock"`
	Value         string `json:"value"`
	Gas           string `json:"gas"`
}

// Vout . N是交易的索引
type Vout struct {
	Address string `json:"Address"`
	Asset   string `json:"Asset"`
	N       int    `json:"N"`
	Value   string `json:"Value"`
}

// UTXO .
type UTXO struct {
	Id          int64  `xorm:"pk autoincr"`
	TX          string `xorm:"notnull index(vout)"`
	N           int    `xorm:"notnull index(vout)"`
	Address     string `xorm:"notnull"`
	CreateBlock int64  `xorm:"notnull index"`
	SpentBlock  int64  `xorm:"notnull index default (-1)"`
	Asset       string `xorm:"notnull index"`
	Value       string `xorm:"notnull"`
	CreateTime  int64  `xorm:"notnull"`
	SpentTime   int64  `xorm:""`
	Claimed     bool   `xorm:""`
}

// UTXO table name
func (table *UTXO) TableName() string {
	return "cam_utxo"
}

func (service *HTTPServer) getPagedOrders(address, asset string, offset, size int) ([]*camdb.Order, error) {

	service.DebugF("get address(%s) orders(%s) (%d,%d)", address, asset, offset, size)

	torders := make([]*camdb.Order, 0)

	err := service.db.
		Where("(`from` = ? or `to` = ?) and asset = ? and (`from` <> `to` )", address, address, asset).
		Desc("create_time").
		Limit(size, offset).
		Find(&torders)

	if err != nil {
		return make([]*camdb.Order, 0), err
	}

	return torders, nil
}

func (service *HTTPServer) createOrder(order *camdb.Order) (int64, error) {

	tOrder := &camdb.Order{
		Id:         order.Id,
		TX:         order.TX,
		From:       order.From,
		To:         order.To,
		Asset:      order.Asset,
		Value:      order.Value,
		CreateTime: order.CreateTime,
		Block:      -1,
		Status:     order.Status,
	}

	_, err := service.db.Insert(tOrder)
	return tOrder.Id, err
}

//根据订单哈希值，返回订单记录，有可能是多条
func (service *HTTPServer) getOrder(tx string) ([]*camdb.Order, error) {

	service.DebugF("get order by tx %s", tx)

	torders := make([]*camdb.Order, 0)

	err := service.db.Where("t_x = ?  and (`from` <> `to`) ", tx).Find(&torders)

	if err != nil {
		return make([]*camdb.Order, 0), err
	}

	return torders, nil
}

//根据订单哈希值，以及行id，返回订单记录
func (service *HTTPServer) getOrderByTxAndId(tx string, tx_id int) ([]*camdb.Order, error) {

	service.DebugF("get order by tx %s", tx)

	torders := make([]*camdb.Order, 0)

	err := service.db.Where("t_x = ? and id = ? ", tx, tx_id).Find(&torders)

	if err != nil {
		return make([]*camdb.Order, 0), err
	}

	return torders, nil
}

//根据地址列表返回订单列表
func (service *HTTPServer) getOrderByAddressList(offset int, size int, address string) ([]*camdb.Order, error) {

	offset = offset * size

	torders := make([]*camdb.Order, 0)

	// var addressQueryStr string
	// //组合查询语句
	// for _,address := range addressList{
	// 	addressQueryStr += "'" + address+"'"+","
	// }

	// addressQueryStr = strings.TrimRight(addressQueryStr,",")

	searchWhere := fmt.Sprintf(" (`from` = '%s' or `to` = '%s') and (`from` <> `to` )", address, address)

	err := service.db.Where(searchWhere).
		Desc("create_time").
		Limit(size, offset).
		Find(&torders)

	if err != nil {
		return make([]*camdb.Order, 0), err
	}

	return torders, nil
}

//根据地址列表返回订单列表
func (service *HTTPServer) getOrderByAddressAssetList(offset int, size int, address string, asset string) ([]*camdb.Order, error) {

	//offset为分页，为页面索引乘以页面包含的数据个数
	offset = offset * size

	torders := make([]*camdb.Order, 0)

	// var addressQueryStr string
	// //组合查询语句
	// for _,address := range addressList{
	// 	addressQueryStr += "'" + address+"'"+","
	// }

	// addressQueryStr = strings.TrimRight(addressQueryStr,",")

	searchWhere := fmt.Sprintf("( `from` ='%s' or `to` = '%s' ) and asset = '%s' and (`from` <> `to` )", address, address, asset)

	err := service.db.Where(searchWhere).
		Desc("create_time").
		Limit(size, offset).
		Find(&torders)

	if err != nil {
		return make([]*camdb.Order, 0), err
	}

	return torders, nil
}

func parsePage(offset string, size string) (*model.Page, error) {
	if offset == "" {
		return nil, fmt.Errorf("offset parameter required")
	}

	if size == "" {
		return nil, fmt.Errorf("size parameter required")
	}

	offseti, err := strconv.ParseUint(offset, 10, 32)

	if err != nil {
		return nil, fmt.Errorf("offset parameter parse failed,%s", err)
	}

	sizei, err := strconv.ParseUint(size, 10, 32)

	if err != nil {
		return nil, fmt.Errorf("size parameter parse failed,%s", err)
	}

	return &model.Page{
		Offset: uint(offseti),
		Size:   uint(sizei),
	}, nil
}

//查询交易状态，如果交易过期后还未确认，则认为失败，如果已经确认则不做处理，
//camindex，会对确认的交易进行推送消息处理
func (service *HTTPServer) checkOrderStatus(txid string) {

	service.DebugF("wait a minute to check order status" + txid)
	//等待30分钟
	time.Sleep(30 * time.Minute)
	// time.Sleep(10*time.Second)

	service.DebugF("check order status" + txid)

	//查询订单状态
	if orders, err := service.getOrder(txid); err != nil {
		service.ErrorF("get orders error :%s", err)
		// ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	} else {

		//增加
		// ctx.JSON(http.StatusOK, orders)
		service.DebugF("find order success")
		if len(orders) > 0 {

			block := orders[0].Block

			if block == -1 {
				//1分钟后未确认，说明失败
				service.DebugF("tx failure :" + txid)

				//更新状态
				//一条交易id有可能包含了多个订单，应更新所有的订单状态
				//存在记录则操作订单
				order := new(camdb.Order)

				//订单失败
				order.Status = 3

				//更新交易状态
				updated, err := service.db.Where("t_x = ?", txid).Cols("status").Update(order)

				//错误则返回错误
				if err != nil {

					service.DebugF("update error %s", err.Error())

					return
				}

				if updated != 0 {

					//更新状态成功
					//则推送通知
					go service.pushToWeb(txid)
				}

			} else {
				service.DebugF("block is %d", block)
				return
			}
		}
	}
}

func (service *HTTPServer) doGetClaim(address string) (*rpc.Unclaimed, error) {

	// logger.DebugF("[doGetClaim]start get claim :%s", address)

	utxos, err := service.unclaimed(address)

	if err != nil {
		return nil, fmt.Errorf("[doGetClaim]get %s get unclaimed utxo err:\n\t%s", address, err)
	}

	// logger.DebugF("[doGetClaim]get address %s unclaimed utxo -- success", address)

	start, end := claim.CalcBlockRange(utxos)

	// service.Debug("[doGetClaim]start get blocks", start, end)
	blocks, err := service.getBlocks(start, end)
	// logger.Debug("[doGetClaim]end get blocks", len(blocks), blocks[len(blocks)-1].Block)

	if err != nil {
		return nil, fmt.Errorf("[doGetClaim]get %s get unclaimed utxo err:\n\t%s", address, err)
	}

	// logger.DebugF("[doGetClaim] calc address %s unclaimed gas", address)

	unavailable, available, err := claim.CalcUnclaimedGas(utxos, blocks)

	if err != nil {
		return nil, fmt.Errorf("[doGetClaim]get address %s unclaimed gas fee err:\n\t%s", address, err)
	}

	// logger.DebugF("[doGetClaim] calc address %s unclaimed gas -- success", address)

	claims := make([]*rpc.UTXO, 0)

	for _, utxo := range utxos {
		if utxo.SpentBlock != -1 {
			claims = append(claims, utxo)
		}
	}

	unclaimed := &rpc.Unclaimed{
		Available:   fmt.Sprintf("%.8f", round(available, 8)),
		Unavailable: fmt.Sprintf("%.8f", round(unavailable, 8)),
		Claims:      claims,
	}

	// jsondata, _ := json.Marshal(unclaimed)

	// service.DebugF("[doGetClaim]finish get claim: %s available: %.8f unavailable: %.8f jsondata: %s", address, round(available, 8), round(unavailable, 8),jsondata)

	return unclaimed, nil
}

func (service *HTTPServer) getBlocksFee(start int64, end int64) (float64, int64, error) {

	var rows []map[string]string
	var err error

	if end == -1 {
		rows, err = service.db.QueryString(`select sum(sys_fee), max(block) from cam_block where block >= ?`, start)
	} else {
		rows, err = service.db.QueryString(`select sum(sys_fee), max(block) from cam_block where block >= ? and block < ?`, start, end)
	}

	if err != nil {
		return 0, end, err
	}

	if len(rows) == 0 {
		return 0, end, nil
	}

	sum, err := strconv.ParseFloat(rows[0]["sum"], 32)

	if err != nil {
		return 0, end, nil
	}

	max, err := strconv.ParseFloat(rows[0]["max"], 32)

	if err != nil {
		return 0, end, nil
	}

	return sum, int64(max), nil
}

func (service *HTTPServer) getBlocks(start int64, end int64) ([]*camdb.Block, error) {

	blocks := make([]*camdb.Block, 0)

	if end == -1 {
		err := service.db.Where(`block >= ?`, start).Find(&blocks)
		if err != nil {
			return nil, err
		}
	} else {
		if err := service.db.Where(`block >= ? and block <= ?`, start, end).Cols("block", "sys_fee").Find(&blocks); err != nil {
			return nil, err
		}
	}

	return blocks, nil
}

func (service *HTTPServer) unclaimed(address string) ([]*rpc.UTXO, error) {

	//cam的资产id
	assetid := "0xceac4961fe81a783516519d263efe4b614777d427b2eccebd1bdb897b705edec"

	utxos := make([]*UTXO, 0)

	err := service.db.
		Where(`asset = ? and address = ? and claimed = 0`, assetid, address).
		Find(&utxos)

	if err != nil {
		return make([]*rpc.UTXO, 0), err
	}

	//组装utxojson
	var utxoJsonAr []*rpc.UTXO

	for _, utxo := range utxos {
		vout := new(rpc.Vout)
		vout.Address = utxo.Address
		vout.Asset = utxo.Asset
		vout.Value = utxo.Value
		vout.N = utxo.N
		utxoJson := new(rpc.UTXO)
		utxoJson.Vout = *vout
		utxoJson.TransactionID = utxo.TX
		utxoJson.CreateTime = string(utxo.CreateTime)
		utxoJson.SpentTime = string(utxo.SpentTime)
		utxoJson.Block = utxo.CreateBlock
		utxoJson.SpentBlock = utxo.SpentBlock
		// utxoJson.Value = utxo.Value
		utxoJson.Gas = "0"

		utxoJsonAr = append(utxoJsonAr, utxoJson)

	}

	return utxoJsonAr, nil

}

func round(f float64, n int) float64 {
	data := fmt.Sprintf("%.9f", f)

	data = data[0 : len(data)-1]

	r, _ := strconv.ParseFloat(data, 8)

	return r
}

//向web后台推送消息
func (etl *HTTPServer) pushToWeb(txid string) {

	//等待3s再推送
	time.Sleep(3 * time.Second)

	// webaddr string,token string,
	webaddr := etl.conf.GetString("order.webaddr", "http://127.0.0.1:5010")

	token := etl.conf.GetString("order.token", "123456789")

	etl.DebugF("push to web %v,txid : %v", webaddr, txid)

	var pushMsg *PushTxMessage = &PushTxMessage{}

	pushMsg.Action = "nodePushMessage"
	pushMsg.AppId = "1536040633"
	pushMsg.Format = "json"
	pushMsg.Method = "avoid"
	pushMsg.Module = "Tool"
	pushMsg.SignMethod = "md5"
	pushMsg.Nonce = string(rand.Intn(100000000))
	pushMsg.TransactionID = txid
	pushMsg.WalletTypeName = "cam"
	pushMsg.Handle = string(rand.Intn(100000000))
	pushMsg.Sign = md5Convert(pushMsg.Nonce + token + pushMsg.Handle)

	body, err := json.Marshal(pushMsg)

	if err != nil {
		return
	}

	if resp, err := http.Post(webaddr,
		"application/json",
		bytes.NewBuffer(body)); err == nil {
		jsonParsed, _ := gabs.ParseJSONBuffer(resp.Body)

		println(pushMsg.TransactionID + "推送成功:" + jsonParsed.String())

	} else {
		println(pushMsg.TransactionID + "推送失败:" + err.Error())
	}
}

func md5Convert(msg string) string {
	h := md5.New()
	h.Write([]byte(msg))
	cipherStr := h.Sum(nil)

	reStr := hex.EncodeToString(cipherStr)

	return reStr
}

type PushTxMessage struct {
	Action         string `json:"action"`
	AppId          string `json:"app_id"`
	Format         string `json:"format"`
	Method         string `json:"method"`
	Module         string `json:"module"`
	SignMethod     string `json:"sign_method"`
	Nonce          string `json:"nonce"`
	TransactionID  string `json:"t_x"`
	WalletTypeName string `json:"wallet_type_name"`
	Handle         string `json:"handle"`
	Sign           string `json:"sign"`
}

//成功返回
func (service *HTTPServer) success(ctx *gin.Context, msg string, data interface{}) {

	var httpResult *HttpResult = &HttpResult{}

	httpResult.Code = 1
	httpResult.Msg = msg
	httpResult.Data = data

	ctx.JSON(http.StatusOK, httpResult)

}

//错误返回
func (service *HTTPServer) error(ctx *gin.Context, msg string) {

	var httpResult *HttpResult = &HttpResult{}

	httpResult.Code = -1
	httpResult.Msg = msg

	ctx.JSON(http.StatusOK, httpResult)

}

func (service *HTTPServer) apiLog(ctx *gin.Context, v interface{}) {
	// server.DebugF
	service.DebugF("c.Request.Method: %v", ctx.Request.Method)
	service.DebugF("c.Request.ContentType: %v", ctx.ContentType())
	service.DebugF("c.Request.Body: %v", ctx.Request.Body)
	service.DebugF("c.Request.ContentLength: %v", ctx.Request.ContentLength)
	data, _ := json.Marshal(v)
	service.DebugF("c.Request.GetBody: %v", string(data))

}

//api请求的返回结构
type HttpResult struct {
	Code int         `json:"code"`
	Msg  string      `json:"msg"`
	Data interface{} `json:"data"`
}

//查询订单，交易哈希值及行id
type SearchOrderByTxAndId struct {
	TX string `json:"tx"`
	Id int    `json:"id"`
}

//查询订单，交易哈希值
type SearchOrderByTx struct {
	TX string `json:"tx"`
}

//查询订单分页，地址列表
type SearchOrderPageAddress struct {
	Offset  int    `json:"offset"`
	Size    int    `json:"size"`
	Address string `json:"address"`
}

//查询订单分页，地址列表，资产id
type SearchOrderPageAddressAsset struct {
	Offset  int    `json:"offset"`
	Size    int    `json:"size"`
	Address string `json:"address"`
	Asset   string `json:"asset"`
}

//增加监控地址
type HttpAddWallet struct {
	Address string `json:"address"`
}

//查询未花资产utxo列表，主要用于交易的参数准备
type HttpUtxoList struct {
	Address string `json:"address"`
	Asset   string `json:"asset"`
}

type syncAddress struct {
	Address string
	Times   int
}

func (address *syncAddress) String() string {
	return fmt.Sprintf("%s (%d)", address.Address, address.Times)
}
