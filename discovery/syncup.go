package discovery

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/bilibili/discovery/conf"
	"github.com/bilibili/discovery/errors"
	"github.com/bilibili/discovery/model"
	"github.com/bilibili/discovery/registry"

	log "github.com/golang/glog"
)

var (
	_fetchAllURL = "http://%s/discovery/fetch/all"
)

// syncUp populates the registry information from a peer eureka node.
func (d *Discovery) syncUp() {
	nodes := d.nodes.Load().(*registry.Nodes)
	for _, node := range nodes.AllNodes() {
		// allNodes 返回本zone的所有节点，包括自己，以及其他zone中的随机一个节点
		if nodes.Myself(node.Addr) {
			continue
		}
		uri := fmt.Sprintf(_fetchAllURL, node.Addr)
		var res struct {
			Code int                          `json:"code"`
			Data map[string][]*model.Instance `json:"data"`
		}
		if err := d.client.Get(context.TODO(), uri, "", nil, &res); err != nil {
			log.Errorf("d.client.Get(%v) error(%v)", uri, err)
			continue
		}
		if res.Code != 0 {
			log.Errorf("service syncup from(%s) failed ", uri)
			continue
		}
		// appid, instance_list
		for _, is := range res.Data {
			for _, i := range is {
				// 此处不太合理，更新一次就 broadcast 一次
				_ = d.registry.Register(i, i.LatestTimestamp)
			}
		}
		// NOTE: no return, make sure that all instances from other nodes register into self.
	}
	nodes.UP()
}

func (d *Discovery) regSelf() context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now().UnixNano()
	ins := &model.Instance{
		Region:   d.c.Env.Region,
		Zone:     d.c.Env.Zone,
		Env:      d.c.Env.DeployEnv,
		Hostname: d.c.Env.Host,
		AppID:    model.AppID,
		Addrs: []string{
			"http://" + d.c.HTTPServer.Addr,
		},
		Status:          model.InstanceStatusUP,
		RegTimestamp:    now,
		UpTimestamp:     now,
		LatestTimestamp: now,
		RenewTimestamp:  now,
		DirtyTimestamp:  now,
	}
	// 注册完自己后，需要将信息复制到其他node
	d.Register(ctx, ins, now, false)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				arg := &model.ArgRenew{
					AppID:    ins.AppID,
					Zone:     d.c.Env.Zone,
					Env:      d.c.Env.DeployEnv,
					Hostname: d.c.Env.Host,
				}
				if _, err := d.Renew(ctx, arg); err != nil && err == errors.NothingFound {
					d.Register(ctx, ins, now, false)
				}
			case <-ctx.Done():
				arg := &model.ArgCancel{
					AppID:    model.AppID,
					Zone:     d.c.Env.Zone,
					Env:      d.c.Env.DeployEnv,
					Hostname: d.c.Env.Host,
				}
				if err := d.Cancel(context.Background(), arg); err != nil {
					log.Errorf("d.Cancel(%+v) error(%v)", arg, err)
				}
				return
			}
		}
	}()
	return cancel
}

func (d *Discovery) nodesproc() {
	var (
		lastTs int64
	)
	for {
		// 不断去 poll 本
		arg := &model.ArgPolls{
			AppID:           []string{model.AppID},
			Zone:            d.c.Env.Zone, // 是否应该传递zone，因为希望返回所有zone的信息
			Env:             d.c.Env.DeployEnv,
			Hostname:        d.c.Env.Host,
			LatestTimestamp: []int64{lastTs},
		}
		ch, _, err := d.registry.Polls(arg)
		if err != nil && err != errors.NotModified {
			log.Errorf("d.registry(%v) error(%v)", arg, err)
			time.Sleep(time.Second)
			continue
		}
		apps := <-ch
		ins, ok := apps[model.AppID]
		if !ok || ins == nil {
			return
		}
		var (
			nodes []string
			zones = make(map[string][]string)
		)
		// zone, ins
		for _, ins := range ins.Instances {
			for _, in := range ins {
				for _, addr := range in.Addrs {
					u, err := url.Parse(addr)
					if err == nil && u.Scheme == "http" {
						if in.Zone == arg.Zone {
							// 自己请求的zone
							nodes = append(nodes, u.Host)
						} else {
							// 其他zone
							zones[in.Zone] = append(zones[in.Zone], u.Host)
						}
					}
				}
			}
		}
		lastTs = ins.LatestTimestamp
		c := new(conf.Config)
		*c = *(d.c)
		c.Nodes = nodes
		c.Zones = zones
		ns := registry.NewNodes(c)
		ns.UP()
		d.nodes.Store(ns)
		log.Infof("discovery changed nodes:%v zones:%v", nodes, zones)
	}
}
