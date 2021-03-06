/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package broadcast

import (
	"io"

	"github.com/hyperledger/fabric/common/flogging"
	"github.com/hyperledger/fabric/common/util"
	"github.com/hyperledger/fabric/orderer/common/msgprocessor"
	cb "github.com/hyperledger/fabric/protos/common"
	ab "github.com/hyperledger/fabric/protos/orderer"
	"github.com/op/go-logging"
	"github.com/pkg/errors"
)

const pkgLogID = "orderer/common/broadcast"

var logger *logging.Logger

func init() {
	logger = flogging.MustGetLogger(pkgLogID)
}

// Handler defines an interface which handles broadcasts
type Handler interface {
	// Handle starts a service thread for a given gRPC connection and services the broadcast connection
	Handle(srv ab.AtomicBroadcast_BroadcastServer) error
}

// ChannelSupportRegistrar provides a way for the Handler to look up the Support for a channel
type ChannelSupportRegistrar interface {
	// BroadcastChannelSupport returns the message channel header, whether the message is a config update
	// and the channel resources for a message or an error if the message is not a message which can
	// be processed directly (like CONFIG and ORDERER_TRANSACTION messages)
	BroadcastChannelSupport(msg *cb.Envelope) (*cb.ChannelHeader, bool, ChannelSupport, error)
}

// ChannelSupport provides the backing resources needed to support broadcast on a channel
type ChannelSupport interface {
	msgprocessor.Processor
	Consenter
}

// Consenter provides methods to send messages through consensus
type Consenter interface {
	// Order accepts a message or returns an error indicating the cause of failure
	// It ultimately passes through to the consensus.Chain interface
	//处理普通交易消息
	Order(env *cb.Envelope, configSeq uint64) error

	// Configure accepts a reconfiguration or returns an error indicating the cause of failure
	// It ultimately passes through to the consensus.Chain interface
	//处理配置交易消息
	Configure(config *cb.Envelope, configSeq uint64) error

	// WaitReady blocks waiting for consenter to be ready for accepting new messages.
	// This is useful when consenter needs to temporarily block ingress messages so
	// that in-flight messages can be consumed. It could return error if consenter is
	// in erroneous states. If this blocking behavior is not desired, consenter could
	// simply return nil.
	//等待共识组件允许接收新消息的信号
	WaitReady() error
}

type handlerImpl struct {
	sm ChannelSupportRegistrar
}

// NewHandlerImpl constructs a new implementation of the Handler interface
func NewHandlerImpl(sm ChannelSupportRegistrar) Handler {
	return &handlerImpl{
		sm: sm,
	}
}

// Handle starts a service thread for a given gRPC connection and services the broadcast connection
//用for循环来接收来自peer节点的消息
func (bh *handlerImpl) Handle(srv ab.AtomicBroadcast_BroadcastServer) error {
	addr := util.ExtractRemoteAddress(srv.Context())
	logger.Debugf("Starting new broadcast loop for %s", addr)
	//消息处理循环
	for {
		//等待接收消息
		//监听提交的交易消息请求
		msg, err := srv.Recv()
		if err == io.EOF {
			logger.Debugf("Received EOF from %s, hangup", addr)
			return nil
		}
		if err != nil {
			logger.Warningf("Error reading from %s: %s", addr, err)
			return err
		}

		//检查消息envelop中的一些字段，比如channelId
		//如果是HeaderType_CONFIG_UPDATE类型的消息，则会将消息经过bh.sm.Process(msg)
		//检查获取的通道头部chdr，配置交易消息标志位isConfig、通道链支持对象（通道消息处理器）
		chdr, isConfig, processor, err := bh.sm.BroadcastChannelSupport(msg)
		if err != nil {
			channelID := "<malformed_header>"
			if chdr != nil {
				channelID = chdr.ChannelId
			}
			logger.Warningf("[channel: %s] Could not get message processor for serving %s: %s", channelID, addr, err)
			return srv.Send(&ab.BroadcastResponse{Status: cb.Status_BAD_REQUEST, Info: err.Error()})
		}

		//检查共识组件是否已经准备好可以接受新交易消息
		//solo共识组件，调用的时候返回nil，表示任何时候都允许Broadcast服务处理句柄接受新的消息
		if err = processor.WaitReady(); err != nil {
			logger.Warningf("[channel: %s] Rejecting broadcast of message from %s with SERVICE_UNAVAILABLE: rejected by Consenter: %s", chdr.ChannelId, addr, err)
			return srv.Send(&ab.BroadcastResponse{Status: cb.Status_SERVICE_UNAVAILABLE, Info: err.Error()})
		}

		//检查是否为配置交易消息
		if !isConfig {
			//普通交易信息
			logger.Debugf("[channel: %s] Broadcast is processing normal message from %s with txid '%s' of type %s", chdr.ChannelId, addr, chdr.TxId, cb.HeaderType_name[chdr.Type])

			//解析获取通道的最新配置序号
			configSeq, err := processor.ProcessNormalMsg(msg)
			if err != nil {
				logger.Warningf("[channel: %s] Rejecting broadcast of normal message from %s because of error: %s", chdr.ChannelId, addr, err)
				return srv.Send(&ab.BroadcastResponse{Status: ClassifyError(err), Info: err.Error()})
			}

			//构造新的普通交易消息并发送到共识组件链对象排序请求处理
			err = processor.Order(msg, configSeq)
			if err != nil {
				logger.Warningf("[channel: %s] Rejecting broadcast of normal message from %s with SERVICE_UNAVAILABLE: rejected by Order: %s", chdr.ChannelId, addr, err)
				return srv.Send(&ab.BroadcastResponse{Status: cb.Status_SERVICE_UNAVAILABLE, Info: err.Error()})
			}
		} else { // isConfig
			//通道配置交易消息：创建或更新应用通道
			logger.Debugf("[channel: %s] Broadcast is processing config update message from %s", chdr.ChannelId, addr)

			//获取配置交易消息与通道的最新配置序号
			config, configSeq, err := processor.ProcessConfigUpdateMsg(msg)
			if err != nil {
				logger.Warningf("[channel: %s] Rejecting broadcast of config message from %s because of error: %s", chdr.ChannelId, addr, err)
				return srv.Send(&ab.BroadcastResponse{Status: ClassifyError(err), Info: err.Error()})
			}

			//构造新的配置交易消息发送到共识组件链对象请求处理
			err = processor.Configure(config, configSeq)
			if err != nil {
				logger.Warningf("[channel: %s] Rejecting broadcast of config message from %s with SERVICE_UNAVAILABLE: rejected by Configure: %s", chdr.ChannelId, addr, err)
				return srv.Send(&ab.BroadcastResponse{Status: cb.Status_SERVICE_UNAVAILABLE, Info: err.Error()})
			}
		}

		logger.Debugf("[channel: %s] Broadcast has successfully enqueued message of type %s from %s", chdr.ChannelId, cb.HeaderType_name[chdr.Type], addr)

		//发送成功处理状态相应消息
		err = srv.Send(&ab.BroadcastResponse{Status: cb.Status_SUCCESS})
		if err != nil {
			logger.Warningf("[channel: %s] Error sending to %s: %s", chdr.ChannelId, addr, err)
			return err
		}
	}
}

// ClassifyError converts an error type into a status code.
func ClassifyError(err error) cb.Status {
	switch errors.Cause(err) {
	case msgprocessor.ErrChannelDoesNotExist:
		return cb.Status_NOT_FOUND
	case msgprocessor.ErrPermissionDenied:
		return cb.Status_FORBIDDEN
	default:
		return cb.Status_BAD_REQUEST
	}
}
