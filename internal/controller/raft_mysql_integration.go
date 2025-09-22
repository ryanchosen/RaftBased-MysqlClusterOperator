package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	databasev1 "github.com/ryansu/api/v1"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Raft与MySQL集成管理器
type RaftMySQLIntegration struct {
	client.Client
	raftManager *RaftManager
	cluster     *databasev1.MysqlCluster
}

// 创建新的Raft-MySQL集成管理器
func NewRaftMySQLIntegration(client client.Client, raftManager *RaftManager, cluster *databasev1.MysqlCluster) *RaftMySQLIntegration {
	return &RaftMySQLIntegration{
		Client:      client,
		raftManager: raftManager,
		cluster:     cluster,
	}
}

// 基于Raft的主库选举
func (rmi *RaftMySQLIntegration) RaftBasedMasterElection(ctx context.Context) (string, []string, error) {
	log := log.FromContext(ctx)
	
	// 等待Raft选举完成
	maxWaitTime := 30 * time.Second
	startTime := time.Now()
	
	for time.Since(startTime) < maxWaitTime {
		rmi.raftManager.mu.RLock()
		if rmi.raftManager.state == databasev1.RaftStateLeader {
			newMaster := rmi.raftManager.nodeId
			rmi.raftManager.mu.RUnlock()
			
			// 获取其他节点作为从库
			nodes, err := rmi.raftManager.getClusterNodes()
			if err != nil {
				return "", nil, err
			}
			
			var slaves []string
			for _, node := range nodes {
				if node != newMaster {
					slaves = append(slaves, node)
				}
			}
			
			log.Info("Raft选举完成", "leader", newMaster, "slaves", slaves)
			return newMaster, slaves, nil
		}
		rmi.raftManager.mu.RUnlock()
		
		time.Sleep(100 * time.Millisecond)
	}
	
	return "", nil, fmt.Errorf("Raft选举超时")
}

// 处理基于Raft的故障转移
func (rmi *RaftMySQLIntegration) HandleRaftBasedFailover(ctx context.Context) error {
	log := log.FromContext(ctx)
	
	// 触发新的Raft选举
	rmi.raftManager.mu.Lock()
	if rmi.raftManager.state != databasev1.RaftStateLeader {
		rmi.raftManager.state = databasev1.RaftStateFollower
		rmi.raftManager.lastHeartbeat = time.Time{} // 强制触发选举
	}
	rmi.raftManager.mu.Unlock()
	
	// 等待选举完成并获取新的主库
	newMaster, slaves, err := rmi.RaftBasedMasterElection(ctx)
	if err != nil {
		return fmt.Errorf("Raft故障转移失败: %v", err)
	}
	
	log.Info("开始基于Raft的MySQL故障转移", "newMaster", newMaster, "slaves", slaves)
	
	// 配置新的MySQL主从关系
	err = rmi.setupMySQLMasterSlaveReplication(ctx, newMaster, slaves)
	if err != nil {
		return fmt.Errorf("配置MySQL主从关系失败: %v", err)
	}
	
	// 配置半同步复制
	err = rmi.ConfigureSemiSyncReplication(ctx, newMaster, slaves)
	if err != nil {
		log.Error(err, "配置半同步复制失败，但故障转移继续")
	}
	
	return nil
}

// 配置MySQL半同步复制
func (rmi *RaftMySQLIntegration) ConfigureSemiSyncReplication(ctx context.Context, masterName string, slaveNames []string) error {
	log := log.FromContext(ctx)
	
	// 配置主库的半同步复制
	err := rmi.configureMasterSemiSync(ctx, masterName)
	if err != nil {
		return fmt.Errorf("配置主库半同步复制失败: %v", err)
	}
	
	// 配置从库的半同步复制
	for _, slaveName := range slaveNames {
		err := rmi.configureSlaveSemiSync(ctx, slaveName)
		if err != nil {
			log.Error(err, "配置从库半同步复制失败", "slave", slaveName)
			// 继续配置其他从库，不中断整个过程
		}
	}
	
	// 验证半同步复制状态
	err = rmi.VerifySemiSyncStatus(ctx, masterName, slaveNames)
	if err != nil {
		log.Error(err, "半同步复制验证失败")
		return err
	}
	
	log.Info("半同步复制配置完成", "master", masterName, "slaves", slaveNames)
	return nil
}

// 配置主库半同步复制
func (rmi *RaftMySQLIntegration) configureMasterSemiSync(ctx context.Context, masterName string) error {
	pod, err := rmi.getPodByName(ctx, masterName)
	if err != nil {
		return err
	}
	
	commands := []string{
		"INSTALL PLUGIN rpl_semi_sync_master SONAME 'semisync_master.so';",
		"SET GLOBAL rpl_semi_sync_master_enabled = 1;",
		"SET GLOBAL rpl_semi_sync_master_timeout = 1000;", // 1秒超时
		"SET GLOBAL rpl_semi_sync_master_wait_for_slave_count = 1;", // 至少等待1个从库确认
	}
	
	for _, cmd := range commands {
		mysqlCmd := fmt.Sprintf("mysql -uroot -p%s -e \"%s\"", MySQLPassword, cmd)
		_, err := rmi.execCommandOnPod(pod, mysqlCmd)
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("执行主库半同步配置命令失败 [%s]: %v", cmd, err)
		}
	}
	
	return nil
}

// 配置从库半同步复制
func (rmi *RaftMySQLIntegration) configureSlaveSemiSync(ctx context.Context, slaveName string) error {
	pod, err := rmi.getPodByName(ctx, slaveName)
	if err != nil {
		return err
	}
	
	commands := []string{
		"INSTALL PLUGIN rpl_semi_sync_slave SONAME 'semisync_slave.so';",
		"SET GLOBAL rpl_semi_sync_slave_enabled = 1;",
		"STOP SLAVE IO_THREAD;",
		"START SLAVE IO_THREAD;",
	}
	
	for _, cmd := range commands {
		mysqlCmd := fmt.Sprintf("mysql -uroot -p%s -e \"%s\"", MySQLPassword, cmd)
		_, err := rmi.execCommandOnPod(pod, mysqlCmd)
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("执行从库半同步配置命令失败 [%s]: %v", cmd, err)
		}
	}
	
	return nil
}

// 验证半同步复制状态
func (rmi *RaftMySQLIntegration) VerifySemiSyncStatus(ctx context.Context, masterName string, slaveNames []string) error {
	// 验证主库半同步状态
	masterPod, err := rmi.getPodByName(ctx, masterName)
	if err != nil {
		return err
	}
	
	masterCmd := fmt.Sprintf("mysql -uroot -p%s -e \"SHOW STATUS LIKE 'Rpl_semi_sync_master_status';\"", MySQLPassword)
	masterResult, err := rmi.execCommandOnPod(masterPod, masterCmd)
	if err != nil {
		return fmt.Errorf("检查主库半同步状态失败: %v", err)
	}
	
	if !strings.Contains(masterResult, "ON") {
		return fmt.Errorf("主库半同步复制未启用")
	}
	
	// 验证从库半同步状态
	for _, slaveName := range slaveNames {
		slavePod, err := rmi.getPodByName(ctx, slaveName)
		if err != nil {
			continue // 跳过无法获取的从库
		}
		
		slaveCmd := fmt.Sprintf("mysql -uroot -p%s -e \"SHOW STATUS LIKE 'Rpl_semi_sync_slave_status';\"", MySQLPassword)
		slaveResult, err := rmi.execCommandOnPod(slavePod, slaveCmd)
		if err != nil {
			continue // 跳过检查失败的从库
		}
		
		if !strings.Contains(slaveResult, "ON") {
			return fmt.Errorf("从库 %s 半同步复制未启用", slaveName)
		}
	}
	
	return nil
}

// 设置MySQL主从复制关系
func (rmi *RaftMySQLIntegration) setupMySQLMasterSlaveReplication(ctx context.Context, masterName string, slaveNames []string) error {
	// 确保新主库的MySQL角色
	err := rmi.ensureMySQLMasterRole(ctx, masterName)
	if err != nil {
		return fmt.Errorf("设置MySQL主库角色失败: %v", err)
	}
	
	// 确保从库的MySQL角色
	for _, slaveName := range slaveNames {
		err := rmi.ensureMySQLSlaveRole(ctx, slaveName, masterName)
		if err != nil {
			return fmt.Errorf("设置MySQL从库角色失败 [%s]: %v", slaveName, err)
		}
	}
	
	return nil
}

// 确保MySQL主库角色
func (rmi *RaftMySQLIntegration) ensureMySQLMasterRole(ctx context.Context, masterName string) error {
	pod, err := rmi.getPodByName(ctx, masterName)
	if err != nil {
		return err
	}
	
	// 重置主库状态
	commands := []string{
		"RESET MASTER;",
		"SET GLOBAL read_only = 0;",
		"SET GLOBAL super_read_only = 0;",
	}
	
	for _, cmd := range commands {
		mysqlCmd := fmt.Sprintf("mysql -uroot -p%s -e \"%s\"", MySQLPassword, cmd)
		_, err := rmi.execCommandOnPod(pod, mysqlCmd)
		if err != nil {
			return fmt.Errorf("执行主库配置命令失败 [%s]: %v", cmd, err)
		}
	}
	
	// 更新Pod标签
	pod.Labels["role"] = "master"
	return rmi.Update(ctx, pod)
}

// 确保MySQL从库角色
func (rmi *RaftMySQLIntegration) ensureMySQLSlaveRole(ctx context.Context, slaveName string, masterName string) error {
	slavePod, err := rmi.getPodByName(ctx, slaveName)
	if err != nil {
		return err
	}
	
	// 停止从库复制
	stopSlaveCmd := fmt.Sprintf("mysql -uroot -p%s -e \"STOP SLAVE;\"", MySQLPassword)
	_, _ = rmi.execCommandOnPod(slavePod, stopSlaveCmd)
	
	// 配置从库连接到新主库
	masterService := fmt.Sprintf("%s.%s.svc.cluster.local", rmi.cluster.Spec.MasterService, rmi.cluster.Namespace)
	changeCmd := fmt.Sprintf(
		"mysql -uroot -p%s -e \"CHANGE MASTER TO MASTER_HOST='%s', MASTER_USER='root', MASTER_PASSWORD='%s', MASTER_AUTO_POSITION=1;\"",
		MySQLPassword, masterService, MySQLPassword,
	)
	
	_, err = rmi.execCommandOnPod(slavePod, changeCmd)
	if err != nil {
		return fmt.Errorf("配置从库主库连接失败: %v", err)
	}
	
	// 启动从库复制
	startSlaveCmd := fmt.Sprintf("mysql -uroot -p%s -e \"START SLAVE;\"", MySQLPassword)
	_, err = rmi.execCommandOnPod(slavePod, startSlaveCmd)
	if err != nil {
		return fmt.Errorf("启动从库复制失败: %v", err)
	}
	
	// 设置从库为只读
	readOnlyCmd := fmt.Sprintf("mysql -uroot -p%s -e \"SET GLOBAL read_only = 1; SET GLOBAL super_read_only = 1;\"", MySQLPassword)
	_, _ = rmi.execCommandOnPod(slavePod, readOnlyCmd)
	
	// 更新Pod标签
	slavePod.Labels["role"] = "slave"
	return rmi.Update(ctx, slavePod)
}

// 获取Pod by name
func (rmi *RaftMySQLIntegration) getPodByName(ctx context.Context, podName string) (*v1.Pod, error) {
	pod := &v1.Pod{}
	err := rmi.Get(ctx, client.ObjectKey{
		Name:      podName,
		Namespace: rmi.cluster.Namespace,
	}, pod)
	return pod, err
}

// 在Pod上执行命令
func (rmi *RaftMySQLIntegration) execCommandOnPod(pod *v1.Pod, command string) (string, error) {
	// 这里应该实现实际的Pod命令执行逻辑
	// 为了简化，这里返回模拟结果
	// 在实际实现中，需要使用Kubernetes client-go的exec功能
	return "", nil
}