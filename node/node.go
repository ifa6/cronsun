package node

import (
	"fmt"
	"os"
	"strconv"
	"time"

	client "github.com/coreos/etcd/clientv3"

	"sunteng/commons/log"
	"sunteng/commons/util"
	"github.com/shunfei/cronsun/conf"
	"github.com/shunfei/cronsun/models"
	"github.com/shunfei/cronsun/node/cron"
)

// Node 执行 cron 命令服务的结构体
type Node struct {
	*models.Client
	*models.Node
	*cron.Cron

	jobs   Jobs // 和结点相关的任务
	groups Groups
	cmds   map[string]*models.Cmd

	link
	// 删除的 job id，用于 group 更新
	delIDs map[string]bool

	ttl  int64
	lID  client.LeaseID // lease id
	done chan struct{}
}

func NewNode(cfg *conf.Conf) (n *Node, err error) {
	ip, err := util.GetLocalIP()
	if err != nil {
		return
	}

	n = &Node{
		Client: models.DefalutClient,
		Node: &models.Node{
			ID:  ip.String(),
			PID: strconv.Itoa(os.Getpid()),
		},
		Cron: cron.New(),

		jobs: make(Jobs, 8),
		cmds: make(map[string]*models.Cmd),

		link:   newLink(8),
		delIDs: make(map[string]bool, 8),

		ttl:  cfg.Ttl,
		done: make(chan struct{}),
	}
	return
}

// 注册到 /cronsun/node/xx
func (n *Node) Register() (err error) {
	pid, err := n.Node.Exist()
	if err != nil {
		return
	}

	if pid != -1 {
		return fmt.Errorf("node[%s] pid[%d] exist", n.Node.ID, pid)
	}

	return n.set()
}

func (n *Node) set() error {
	resp, err := n.Client.Grant(n.ttl + 2)
	if err != nil {
		return err
	}

	if _, err = n.Node.Put(client.WithLease(resp.ID)); err != nil {
		return err
	}

	n.lID = resp.ID
	return nil
}

// 断网掉线重新注册
func (n *Node) keepAlive() {
	duration := time.Duration(n.ttl) * time.Second
	timer := time.NewTimer(duration)
	for {
		select {
		case <-n.done:
			return
		case <-timer.C:
			if n.lID > 0 {
				_, err := n.Client.KeepAliveOnce(n.lID)
				if err == nil {
					timer.Reset(duration)
					continue
				}

				log.Warnf("%s lid[%x] keepAlive err: %s, try to reset...", n.String(), n.lID, err.Error())
				n.lID = 0
			}

			if err := n.set(); err != nil {
				log.Warnf("%s set lid err: %s, try to reset after %d seconds...", n.String(), err.Error(), n.ttl)
			} else {
				log.Noticef("%s set lid[%x] success", n.String(), n.lID)
			}
			timer.Reset(duration)
		}
	}
}

func (n *Node) loadJobs() (err error) {
	if n.groups, err = models.GetGroups(""); err != nil {
		return
	}

	jobs, err := models.GetJobs()
	if err != nil {
		return
	}

	if len(jobs) == 0 {
		return
	}

	for _, job := range jobs {
		job.RunOn(n.ID)
		n.addJob(job, false)
	}

	return
}

func (n *Node) addJob(job *models.Job, notice bool) {
	n.link.addJob(job)
	if job.IsRunOn(n.ID, n.groups) {
		n.jobs[job.ID] = job
	}

	cmds := job.Cmds(n.ID, n.groups)
	if len(cmds) == 0 {
		return
	}

	for _, cmd := range cmds {
		n.addCmd(cmd, notice)
	}
	return
}

func (n *Node) delJob(id string) {
	n.delIDs[id] = true
	job, ok := n.jobs[id]
	// 之前此任务没有在当前结点执行
	if !ok {
		return
	}

	delete(n.jobs, id)
	n.link.delJob(job)

	cmds := job.Cmds(n.ID, n.groups)
	if len(cmds) == 0 {
		return
	}

	for _, cmd := range cmds {
		n.delCmd(cmd)
	}
	return
}

func (n *Node) modJob(job *models.Job) {
	oJob, ok := n.jobs[job.ID]
	// 之前此任务没有在当前结点执行，直接增加任务
	if !ok {
		n.addJob(job, true)
		return
	}

	n.link.delJob(oJob)
	prevCmds := oJob.Cmds(n.ID, n.groups)
	*oJob = *job
	cmds := oJob.Cmds(n.ID, n.groups)

	for id, cmd := range cmds {
		n.addCmd(cmd, true)
		delete(prevCmds, id)
	}

	for _, cmd := range prevCmds {
		n.delCmd(cmd)
	}

	n.link.addJob(oJob)
}

func (n *Node) addCmd(cmd *models.Cmd, notice bool) {
	c, ok := n.cmds[cmd.GetID()]
	if ok {
		sch := c.JobRule.Timer
		*c = *cmd

		// 节点执行时间不变，不用更新 cron
		if c.JobRule.Timer == sch {
			return
		}
	} else {
		c = cmd
	}

	n.Cron.Schedule(c.JobRule.Schedule, c)
	if !ok {
		n.cmds[c.GetID()] = c
	}

	if notice {
		log.Noticef("job[%s] rule[%s] timer[%s] has added", c.Job.ID, c.JobRule.ID, c.JobRule.Timer)
	}
	return
}

func (n *Node) delCmd(cmd *models.Cmd) {
	delete(n.cmds, cmd.GetID())
	n.Cron.DelJob(cmd)
	log.Noticef("job[%s] rule[%s] timer[%s] has deleted", cmd.Job.ID, cmd.JobRule.ID, cmd.JobRule.Timer)
}

func (n *Node) addGroup(g *models.Group) {
	n.groups[g.ID] = g
}

func (n *Node) delGroup(id string) {
	delete(n.groups, id)
	n.link.delGroup(id)

	job, ok := n.jobs[id]
	// 之前此任务没有在当前结点执行
	if !ok {
		return
	}

	cmds := job.Cmds(n.ID, n.groups)
	if len(cmds) == 0 {
		return
	}

	for _, cmd := range cmds {
		n.delCmd(cmd)
	}
	return
}

func (n *Node) modGroup(g *models.Group) {
	oGroup, ok := n.groups[g.ID]
	if !ok {
		n.addGroup(g)
		return
	}

	// 都包含/都不包含当前节点，对当前节点任务无影响
	if (oGroup.Included(n.ID) && g.Included(n.ID)) || (!oGroup.Included(n.ID) && !g.Included(n.ID)) {
		*oGroup = *g
		return
	}

	// 增加当前节点
	if !oGroup.Included(n.ID) && g.Included(n.ID) {
		n.groupAddNode(g)
		return
	}

	// 移除当前节点
	n.groupRmNode(g, oGroup)
	return
}

func (n *Node) groupAddNode(g *models.Group) {
	n.groups[g.ID] = g
	jls := n.link[g.ID]
	if len(jls) == 0 {
		return
	}

	var err error
	for jid, jl := range jls {
		job, ok := n.jobs[jid]
		if !ok {
			// job 已删除
			if n.delIDs[jid] {
				n.link.delGroupJob(g.ID, jid)
				continue
			}

			if job, err = models.GetJob(jl.gname, jid); err != nil {
				log.Warnf("get job[%s][%s] err: %s", jl.gname, jid, err.Error())
				n.link.delGroupJob(g.ID, jid)
				continue
			}
		}

		cmds := job.Cmds(n.ID, n.groups)
		for _, cmd := range cmds {
			n.addCmd(cmd, true)
		}
	}
	return
}

func (n *Node) groupRmNode(g, og *models.Group) {
	jls := n.link[g.ID]
	if len(jls) == 0 {
		n.groups[g.ID] = g
		return
	}

	for jid, _ := range jls {
		job, ok := n.jobs[jid]
		// 之前此任务没有在当前结点执行
		if !ok {
			n.link.delGroupJob(g.ID, jid)
			continue
		}

		n.groups[og.ID] = og
		prevCmds := job.Cmds(n.ID, n.groups)
		n.groups[g.ID] = g
		cmds := job.Cmds(n.ID, n.groups)

		for id, cmd := range cmds {
			n.addCmd(cmd, true)
			delete(prevCmds, id)
		}

		for _, cmd := range prevCmds {
			n.delCmd(cmd)
		}
	}

	n.groups[g.ID] = g
}

func (n *Node) watchJobs() {
	rch := models.WatchJobs()
	for wresp := range rch {
		for _, ev := range wresp.Events {
			switch {
			case ev.IsCreate():
				job, err := models.GetJobFromKv(ev.Kv)
				if err != nil {
					log.Warnf("err: %s, kv: %s", err.Error(), ev.Kv.String())
					continue
				}

				job.RunOn(n.ID)
				n.addJob(job, true)
			case ev.IsModify():
				job, err := models.GetJobFromKv(ev.Kv)
				if err != nil {
					log.Warnf("err: %s, kv: %s", err.Error(), ev.Kv.String())
					continue
				}

				job.RunOn(n.ID)
				n.modJob(job)
			case ev.Type == client.EventTypeDelete:
				n.delJob(models.GetIDFromKey(string(ev.Kv.Key)))
			default:
				log.Warnf("unknown event type[%v] from job[%s]", ev.Type, string(ev.Kv.Key))
			}
		}
	}
}

func (n *Node) watchGroups() {
	rch := models.WatchGroups()
	for wresp := range rch {
		for _, ev := range wresp.Events {
			switch {
			case ev.IsCreate():
				g, err := models.GetGroupFromKv(ev.Kv)
				if err != nil {
					log.Warnf("err: %s, kv: %s", err.Error(), ev.Kv.String())
					continue
				}

				n.addGroup(g)
			case ev.IsModify():
				g, err := models.GetGroupFromKv(ev.Kv)
				if err != nil {
					log.Warnf("err: %s, kv: %s", err.Error(), ev.Kv.String())
					continue
				}

				n.modGroup(g)
			case ev.Type == client.EventTypeDelete:
				n.delGroup(models.GetIDFromKey(string(ev.Kv.Key)))
			default:
				log.Warnf("unknown event type[%v] from group[%s]", ev.Type, string(ev.Kv.Key))
			}
		}
	}
}

func (n *Node) watchOnce() {
	rch := models.WatchOnce()
	for wresp := range rch {
		for _, ev := range wresp.Events {
			switch {
			case ev.IsCreate(), ev.IsModify():
				if len(ev.Kv.Value) != 0 && string(ev.Kv.Value) != n.ID {
					continue
				}

				job, ok := n.jobs[models.GetIDFromKey(string(ev.Kv.Key))]
				if !ok || !job.IsRunOn(n.ID, n.groups) {
					continue
				}

				go job.RunWithRecovery()
			}
		}
	}
}

// 启动服务
func (n *Node) Run() (err error) {
	go n.keepAlive()

	defer func() {
		if err != nil {
			n.Stop(nil)
		}
	}()

	if err = n.loadJobs(); err != nil {
		return
	}

	n.Cron.Start()
	go n.watchJobs()
	go n.watchGroups()
	go n.watchOnce()
	n.Node.On()
	return
}

// 停止服务
func (n *Node) Stop(i interface{}) {
	n.Node.Down()
	close(n.done)
	n.Node.Del()
	n.Client.Close()
	n.Cron.Stop()
}
