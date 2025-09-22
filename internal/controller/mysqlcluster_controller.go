package controller

import (
	"context"                         // 用于处理上下文，提供超时、取消等操作
	"fmt"                             // 用于字符串格式化
	"github.com/go-logr/logr"         // 用于记录日志
	"k8s.io/apimachinery/pkg/runtime" // 提供对象的通用机制，如序列化和版本转换

	"sigs.k8s.io/controller-runtime/pkg/client" // 提供与 Kubernetes API 交互的客户端
	"sigs.k8s.io/controller-runtime/pkg/log"

	databasev1 "github.com/ryansu/api/v1" // 导入自定义的 MySQLCluster API 资源定义
	v1 "k8s.io/api/core/v1"                // 核心 Kubernetes API 对象，例如 Pod 和 Service
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	MySQLPassword          = "password"           // Hardcoded MySQL password
	//KubeConfigPath         = "/root/.kube/config" // Hardcoded kubeconfig path
	MysqlClusterKind       = "MysqlCluster"
	MysqlClusterAPIVersion = "apps.ryansu.com/v1"
)

// MysqlClusterReconciler reconciles a MysqlCluster object
type MysqlClusterReconciler struct {
	client.Client                  // 嵌入 client.Client 接口，用于与 Kubernetes API 交互
	Log                logr.Logger // 日志记录器
	Scheme             *runtime.Scheme
	MasterGTIDSnapshot string // 用于存储主库的 GTID 快照
	SnapGoIsEnabled    bool   // 标识用于记录GTID快照的协程序是否启动，默认值为false，只有启动后才会设置为true
	RaftManager        *RaftManager // Raft管理器
	RaftIntegration    *RaftMySQLIntegration // Raft与MySQL集成
}

/*
在您的代码中，涉及到的 Kubernetes 资源包括：Pod、ConfigMap、Service、Endpoints、namespace对应设置权限如下
*/
// +kubebuilder:rbac:groups=apps.ryansu.com,resources=mysqlclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.ryansu.com,resources=mysqlclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.ryansu.com,resources=mysqlclusters/finalizers,verbs=update

// +kubebuilder:rbac:groups="",resources=pods;services;configmaps,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create;get;list;watch
// +kubebuilder:rbac:groups="",resources=endpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;create;update;delete

// 调谐函数
func (r *MysqlClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("调谐函数触发执行", "req", req) // 额外增加1个字段

	var cluster databasev1.MysqlCluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 检查是否已经初始化
	if _, ok := cluster.Annotations["initialized"]; !ok {
		// 未初始，则调用初始化函数
		if err := r.init(ctx, &cluster); err != nil {
			log.Info("初始化集群失败")
			return ctrl.Result{}, err
		} else {
			log.Info("初始化集群成功")
		}

		// 设置 annotation 表示初始化已完成
		if cluster.Annotations == nil {
			cluster.Annotations = make(map[string]string)
		}
		cluster.Annotations["initialized"] = "true"
		if err := r.Update(ctx, &cluster); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		// 已经初始完成，则进入检测逻辑
		
	// 初始化Raft管理器（如果还未初始化）
		if r.RaftManager == nil {
			err := r.initializeRaft(ctx, &cluster)
			if err != nil {
				log.Error(err, "初始化Raft管理器失败")
				return ctrl.Result{}, err
			}
			
			// 启动Raft监控
			monitor := NewRaftMonitor(r.Client, r.RaftManager, &cluster)
			monitor.Start(ctx)
		}
		
		// 1、副本调谐
		result, err := r.reconcileReplicas(ctx, cluster)
		if err != nil {
			return result, err
		}
		
		// 2、基于Raft的主从检测逻辑与调谐
		result, err = r.reconcileRaftBasedMasterSlave(ctx, cluster)
		if err != nil {
			return result, err
		}
		
		// 3、确保半同步复制配置
		err = r.ensureSemiSyncReplication(ctx, cluster)
		if err != nil {
			log.Error(err, "配置半同步复制失败")
			// 不中断调谐过程，继续执行
		}
	}

	// 启用协程定期记录当前主库的GTID快照，用于选举依据
	if !r.SnapGoIsEnabled {
		r.startAndUpdateGTIDSnapshot(ctx, cluster)
		r.SnapGoIsEnabled = true
	}

	return ctrl.Result{}, nil
}

// 在cmd/main.go入口main函数中会调用该函数，来对你的控制器进行设置，指定控制器管理的资源，并将控制器注册到控制器管理器中
func (r *MysqlClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// 增加：Owns(&v1.Pod{})，确保检测pod资源
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasev1.MysqlCluster{}).
		Owns(&v1.Pod{}).
		Complete(r)
}
// bug说明
// 上述SetupWithManager的核心在与针对MysqlCluster资源所拥有的Pod控制器只会在 MysqlCluster 对象本身发生 创建 / 更新 / 删除 时被触发
// 我检测当pod发生挂掉时，会触发调谐函数的执行，检查主从状态的逻辑是根据endpoint来判断的，如果endpoint控制器摘出失效的ednpoint的慢了（异步执行的），会导致我的调谐逻辑运行失效，所有从库的主从状态都挂掉，
// 然而后续也不会有事件出来了，这就会导致主从状态无法恢复正常，这个该如何解决呢？
// 在SetupWithManager中添加对相关endpoint的watch事件，例如下面的样子，代码没有经过测试，只罗列大致逻辑
// func (r *MysqlClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
//     return ctrl.NewControllerManagedBy(mgr).
//         For(&databasev1.MysqlCluster{}).
//         Owns(&corev1.Pod{}).
//         Watches(&source.Kind{Type: &corev1.Endpoints{}}, &handler.EnqueueRequestForOwner{
//             OwnerType:    &databasev1.MysqlCluster{},
//             IsController: true,
//         }).
//         Complete(r)
// }
// 这样：
// （1）Pod 变动触发 Reconcile
// （2）Endpoint 更新/删除 也会触发 Reconcile
// → 你就不会错过 Endpoint 状态更新。

// 初始化Raft管理器
func (r *MysqlClusterReconciler) initializeRaft(ctx context.Context, cluster *databasev1.MysqlCluster) error {
	log := log.FromContext(ctx)
	
	// 获取当前Pod名称作为节点ID
	nodeId := r.getCurrentNodeId(cluster)
	
	// 创建Raft管理器
	r.RaftManager = NewRaftManager(r.Client, nodeId, cluster)
	
	// 创建Raft与MySQL集成管理器
	r.RaftIntegration = NewRaftMySQLIntegration(r.Client, r.RaftManager, cluster)
	
	// 启动Raft节点
	r.RaftManager.Start(ctx)
	
	log.Info("Raft管理器初始化完成", "nodeId", nodeId)
	return nil
}

// 基于Raft的主从调谐逻辑
func (r *MysqlClusterReconciler) reconcileRaftBasedMasterSlave(ctx context.Context, cluster databasev1.MysqlCluster) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	
	// 检查当前Raft状态
	r.RaftManager.mu.RLock()
	currentState := r.RaftManager.state
	currentLeader := r.RaftManager.nodeId
	r.RaftManager.mu.RUnlock()
	
	// 检查MySQL主库状态
	masterAlive, err := r.checkMasterStatus(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	
	if !masterAlive {
		log.Info("检测到主库故障，开始基于Raft的故障转移")
		
		// 使用Raft进行故障转移
		err := r.RaftIntegration.HandleRaftBasedFailover(ctx)
		if err != nil {
			log.Error(err, "基于Raft的故障转移失败")
			return ctrl.Result{}, err
		}
		
		log.Info("基于Raft的故障转移完成")
	} else if currentState == databasev1.RaftStateLeader {
		// 如果当前节点是Raft领导者，确保MySQL主从关系正确
		masterPodName, failedReplicas, err := r.checkReplicaStatus(ctx, cluster)
		if err != nil {
			return ctrl.Result{}, err
		}
		
		// 重新配置失败的从库
		if len(failedReplicas) > 0 {
			if err := r.setupMasterSlaveReplication(ctx, masterPodName, failedReplicas, cluster); err != nil {
				return ctrl.Result{}, err
			}
		}
		
		// 确保所有非主库的 Pod 标签都是 slave
		if err := r.ensureSlaveRoles(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
	}
	
	return ctrl.Result{}, nil
}

// 确保半同步复制配置
func (r *MysqlClusterReconciler) ensureSemiSyncReplication(ctx context.Context, cluster databasev1.MysqlCluster) error {
	if r.RaftIntegration == nil {
		return nil // Raft集成未初始化，跳过
	}
	
	// 获取当前主库和从库
	masterName := cluster.Status.Master
	if masterName == "" {
		return nil // 主库未确定，跳过
	}
	
	slaves := cluster.Status.Slaves
	if len(slaves) == 0 {
		return nil // 没有从库，跳过
	}
	
	// 配置半同步复制
	return r.RaftIntegration.ConfigureSemiSyncReplication(ctx, masterName, slaves)
}

// 获取当前节点ID
func (r *MysqlClusterReconciler) getCurrentNodeId(cluster *databasev1.MysqlCluster) string {
	// 在实际部署中，这应该从环境变量或Pod名称获取
	// 这里使用简化的逻辑，假设控制器运行在第一个Pod中
	return fmt.Sprintf("%s-0", cluster.Name)
}
