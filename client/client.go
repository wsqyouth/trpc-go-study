//
//
// Tencent is pleased to support the open source community by making tRPC available.
//
// Copyright (C) 2023 THL A29 Limited, a Tencent company.
// All rights reserved.
//
// If you have downloaded a copy of the tRPC source code from Tencent,
// please note that tRPC source code is licensed under the  Apache 2.0 License,
// A copy of the Apache 2.0 License is included in this file.
//
//

// Package client is tRPC-Go clientside implementation,
// including network transportation, resolving, routing etc.
package client

import (
	"context"
	"fmt"
	"net"
	"time"

	"trpc.group/trpc-go/trpc-go/codec"
	"trpc.group/trpc-go/trpc-go/errs"
	"trpc.group/trpc-go/trpc-go/filter"
	"trpc.group/trpc-go/trpc-go/internal/attachment"
	icodec "trpc.group/trpc-go/trpc-go/internal/codec"
	"trpc.group/trpc-go/trpc-go/internal/report"
	"trpc.group/trpc-go/trpc-go/naming/registry"
	"trpc.group/trpc-go/trpc-go/naming/selector"
	"trpc.group/trpc-go/trpc-go/rpcz"
	"trpc.group/trpc-go/trpc-go/transport"
)

// Client is the interface that initiates RPCs and sends request messages to a server.
type Client interface {
	// Invoke performs a unary RPC.
	Invoke(ctx context.Context, reqBody interface{}, rspBody interface{}, opt ...Option) error
}

// DefaultClient is the default global client.
// It's thread-safe.
// NOTES: DefaultClient 是 tRPC Go 桩代码中的默认客户端，单例模式，它是线程安全的。
var DefaultClient = New()

// New creates a client that uses default client transport.
var New = func() Client {
	return &client{}
}

// client is the default implementation of Client with
// pluggable codec, transport, filter etc.
type client struct{}

// Invoke invokes a backend call by passing in custom request/response message
// and running selector filter, codec, transport etc.
// NOTES: Invoke 是客户端发起 RPC 的入口，它会执行 selector filter、codec、transport 等
//  	这个方法是整个 RPC 调用过程的核心
func (c *client) Invoke(ctx context.Context, reqBody interface{}, rspBody interface{}, opt ...Option) (err error) {
	// The generic message structure data of the current request is retrieved from the context,
	// and each backend call uses a new msg generated by the client stub code.
	// 重点: msg属性是很重要的,可以通俗的理解为是header
	ctx, msg := codec.EnsureMessage(ctx)

	span, end, ctx := rpcz.NewSpanContext(ctx, "client")

	// Get client options.
	opts, err := c.getOptions(msg, opt...)
	defer func() {
		span.SetAttribute(rpcz.TRPCAttributeRPCName, msg.ClientRPCName())
		if err == nil {
			span.SetAttribute(rpcz.TRPCAttributeError, msg.ClientRspErr())
		} else {
			span.SetAttribute(rpcz.TRPCAttributeError, err)
		}
		end.End()
	}()
	if err != nil {
		return err
	}

	// Update Msg by options.
	c.updateMsg(msg, opts)

	fullLinkDeadline, ok := ctx.Deadline()
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout) // 设置超时
		defer cancel()
	}
	if deadline, ok := ctx.Deadline(); ok {
		msg.WithRequestTimeout(deadline.Sub(time.Now()))
	}
	if ok && (opts.Timeout <= 0 || time.Until(fullLinkDeadline) < opts.Timeout) {
		opts.fixTimeout = mayConvert2FullLinkTimeout
	}

	// Start filter chain processing.
	filters := c.fixFilters(opts)
	span.SetAttribute(rpcz.TRPCAttributeFilterNames, opts.FilterNames)
	// 重点: 处理filter,callFunc处理发送逻辑
	// NOTES: 核心中的核心在于 callFunc 函数
	return filters.Filter(contextWithOptions(ctx, opts), reqBody, rspBody, callFunc)
}

// getOptions returns Options needed by each RPC.
func (c *client) getOptions(msg codec.Msg, opt ...Option) (*Options, error) {
	opts := getOptionsByCalleeAndUserOptions(msg.CalleeServiceName(), opt...).clone()

	// Set service info options.
	opts.SelectOptions = append(opts.SelectOptions, c.getServiceInfoOptions(msg)...)

	// The given input options have the highest priority
	// and they will override the original ones.
	for _, o := range opt {
		o(opts)
	}

	if err := opts.parseTarget(); err != nil {
		return nil, errs.NewFrameError(errs.RetClientRouteErr, err.Error())
	}
	return opts, nil
}

// getServiceInfoOptions returns service info options.
func (c *client) getServiceInfoOptions(msg codec.Msg) []selector.Option {
	if msg.Namespace() != "" {
		return []selector.Option{
			selector.WithSourceNamespace(msg.Namespace()),
			selector.WithSourceServiceName(msg.CallerServiceName()),
			selector.WithSourceEnvName(msg.EnvName()),
			selector.WithEnvTransfer(msg.EnvTransfer()),
			selector.WithSourceSetName(msg.SetName()),
		}
	}
	return nil
}

// updateMsg updates msg.
func (c *client) updateMsg(msg codec.Msg, opts *Options) {
	// Set callee service name.
	// Generally, service name is the same as the package.service defined in proto file,
	// but it can be customized by options.
	if opts.ServiceName != "" {
		// From client's perspective, caller refers to itself, callee refers to the backend service.
		msg.WithCalleeServiceName(opts.ServiceName)
	}
	if opts.endpoint == "" {
		// If endpoint is not configured, DefaultSelector (generally polaris)
		// will be used to address callee service name.
		opts.endpoint = msg.CalleeServiceName()
	}
	if opts.CalleeMethod != "" {
		msg.WithCalleeMethod(opts.CalleeMethod)
	}

	// Set metadata.
	if len(opts.MetaData) > 0 {
		msg.WithClientMetaData(c.getMetaData(msg, opts))
	}

	// Set caller service name if needed.
	if opts.CallerServiceName != "" {
		msg.WithCallerServiceName(opts.CallerServiceName)
	}
	if icodec.IsValidSerializationType(opts.SerializationType) {
		msg.WithSerializationType(opts.SerializationType)
	}
	if icodec.IsValidCompressType(opts.CompressType) && opts.CompressType != codec.CompressTypeNoop {
		msg.WithCompressType(opts.CompressType)
	}

	// Set client req head if needed.
	if opts.ReqHead != nil {
		msg.WithClientReqHead(opts.ReqHead)
	}
	// Set client rsp head if needed.
	if opts.RspHead != nil {
		msg.WithClientRspHead(opts.RspHead)
	}

	msg.WithCallType(opts.CallType)

	if opts.attachment != nil {
		setAttachment(msg, opts.attachment)
	}
}

// SetAttachment sets attachment to msg.
func setAttachment(msg codec.Msg, attm *attachment.Attachment) {
	cm := msg.CommonMeta()
	if cm == nil {
		cm = make(codec.CommonMeta)
		msg.WithCommonMeta(cm)
	}
	cm[attachment.ClientAttachmentKey{}] = attm
}

// getMetaData returns metadata that will be transparently transmitted to the backend service.
func (c *client) getMetaData(msg codec.Msg, opts *Options) codec.MetaData {
	md := msg.ClientMetaData()
	if md == nil {
		md = codec.MetaData{}
	}
	for k, v := range opts.MetaData {
		md[k] = v
	}
	return md
}

func (c *client) fixFilters(opts *Options) filter.ClientChain {
	if opts.DisableFilter || len(opts.Filters) == 0 {
		// All filters but selector filter are disabled.
		opts.FilterNames = append(opts.FilterNames, DefaultSelectorFilterName) // NOTES: 这里可以看到，selectorFilter 是必须的且是自动注入的
		return filter.ClientChain{selectorFilter}
	}
	if !opts.selectorFilterPosFixed {
		// Selector filter pos is not fixed, append it to the filter chain.
		opts.Filters = append(opts.Filters, selectorFilter)
		opts.FilterNames = append(opts.FilterNames, DefaultSelectorFilterName)
	}
	return opts.Filters
}

// callFunc is the function that calls the backend service with
// codec encoding/decoding and network transportation.
// Filters executed before this function are called prev filters. Filters executed after
// this function are called post filters.
// NOTES: callFunc 定义了 RPC 调用过程中的核心逻辑，包括序列化、编解码、网络传输等
func callFunc(ctx context.Context, reqBody interface{}, rspBody interface{}) (err error) {
	msg := codec.Message(ctx)
	opts := OptionsFromContext(ctx)

	defer func() { err = opts.fixTimeout(err) }()

	// Check if codec is empty, after updating msg.
	if opts.Codec == nil {
		report.ClientCodecEmpty.Incr()
		return errs.NewFrameError(errs.RetClientEncodeFail, "client: codec empty")
	}

	// NOTES: 发送前预处理 负责完整的序列化、压缩、编码工作，是请求前的准备工作
	reqBuf, err := prepareRequestBuf(ctx, msg, reqBody, opts)
	if err != nil {
		return err
	}

	// Call backend service.
	if opts.EnableMultiplexed {
		opts.CallOptions = append(opts.CallOptions, transport.WithMsg(msg), transport.WithMultiplexed(true))
	}
	// NOTES: 发送主逻辑,获取返回包 是请求传输过程
	rspBuf, err := opts.Transport.RoundTrip(ctx, reqBuf, opts.CallOptions...)
	if err != nil {
		if err == errs.ErrClientNoResponse { // Sendonly mode, no response, just return nil.
			return nil
		}
		return err
	}

	span := rpcz.SpanFromContext(ctx)
	span.SetAttribute(rpcz.TRPCAttributeResponseSize, len(rspBuf))
	_, end := span.NewChild("DecodeProtocolHead")
	// NOTES: Decode 完成响应包的解码过程
	rspBodyBuf, err := opts.Codec.Decode(msg, rspBuf)
	end.End()
	if err != nil {
		return errs.NewFrameError(errs.RetClientDecodeFail, "client codec Decode: "+err.Error())
	}
	// 处理返回包
	// NOTES: processResponseBuf 完成响应消息的解压缩、反序列化工作
	return processResponseBuf(ctx, msg, rspBody, rspBodyBuf, opts)
}

func prepareRequestBuf(
	ctx context.Context,
	msg codec.Msg,
	reqBody interface{},
	opts *Options,
) ([]byte, error) {
	reqBodyBuf, err := serializeAndCompress(ctx, msg, reqBody, opts) // NOTES: 序列化和压缩
	if err != nil {
		return nil, err
	}

	// Encode the whole reqBodyBuf.
	span := rpcz.SpanFromContext(ctx)
	_, end := span.NewChild("EncodeProtocolHead")
	reqBuf, err := opts.Codec.Encode(msg, reqBodyBuf) // NOTES: 编码
	end.End()
	span.SetAttribute(rpcz.TRPCAttributeRequestSize, len(reqBuf))
	if err != nil {
		return nil, errs.NewFrameError(errs.RetClientEncodeFail, "client codec Encode: "+err.Error())
	}

	return reqBuf, nil
}

func processResponseBuf(
	ctx context.Context,
	msg codec.Msg,
	rspBody interface{},
	rspBodyBuf []byte,
	opts *Options,
) error {
	// Error from response.
	if msg.ClientRspErr() != nil {
		return msg.ClientRspErr()
	}

	if len(rspBodyBuf) == 0 {
		return nil
	}

	// Decompress.
	span := rpcz.SpanFromContext(ctx)
	_, end := span.NewChild("Decompress")
	compressType := msg.CompressType()
	if icodec.IsValidCompressType(opts.CurrentCompressType) { // NOTES: 解压缩
		compressType = opts.CurrentCompressType
	}
	var err error
	if icodec.IsValidCompressType(compressType) && compressType != codec.CompressTypeNoop {
		rspBodyBuf, err = codec.Decompress(compressType, rspBodyBuf)
	}
	end.End()
	if err != nil {
		return errs.NewFrameError(errs.RetClientDecodeFail, "client codec Decompress: "+err.Error())
	}

	// unmarshal rspBodyBuf to rspBody.
	_, end = span.NewChild("Unmarshal")
	serializationType := msg.SerializationType()
	if icodec.IsValidSerializationType(opts.CurrentSerializationType) {
		serializationType = opts.CurrentSerializationType
	}
	if icodec.IsValidSerializationType(serializationType) { // NOTES: 反序列化
		err = codec.Unmarshal(serializationType, rspBodyBuf, rspBody)
	}

	end.End()
	if err != nil {
		return errs.NewFrameError(errs.RetClientDecodeFail, "client codec Unmarshal: "+err.Error())
	}

	return nil
}

// serializeAndCompress serializes and compresses reqBody.
func serializeAndCompress(ctx context.Context, msg codec.Msg, reqBody interface{}, opts *Options) ([]byte, error) {
	// Marshal reqBody into binary body.
	span := rpcz.SpanFromContext(ctx)
	_, end := span.NewChild("Marshal")
	serializationType := msg.SerializationType() // 获取序列化类型 桩代码中通过 msg.WithSerializationType 设置的序列化类型
	// opts提供了一种运行时覆盖序列号格式及压缩格式的机制。
	if icodec.IsValidSerializationType(opts.CurrentSerializationType) {
		serializationType = opts.CurrentSerializationType // 注: 可以看到非法时使用默认值, 未报错
	}
	var (
		reqBodyBuf []byte
		err        error
	)
	if icodec.IsValidSerializationType(serializationType) {
		reqBodyBuf, err = codec.Marshal(serializationType, reqBody)
	}
	end.End()
	if err != nil {
		return nil, errs.NewFrameError(errs.RetClientEncodeFail, "client codec Marshal: "+err.Error())
	}

	// Compress.
	_, end = span.NewChild("Compress")
	compressType := msg.CompressType() // 获取压缩类型
	if icodec.IsValidCompressType(opts.CurrentCompressType) {
		compressType = opts.CurrentCompressType // 注: 可以看到非法时使用默认值, 未报错
	}
	if icodec.IsValidCompressType(compressType) && compressType != codec.CompressTypeNoop {
		reqBodyBuf, err = codec.Compress(compressType, reqBodyBuf)
	}
	end.End()
	if err != nil {
		return nil, errs.NewFrameError(errs.RetClientEncodeFail, "client codec Compress: "+err.Error())
	}
	return reqBodyBuf, nil
}

// -------------------------------- client selector filter ------------------------------------- //

// selectorFilter is the client selector filter.
func selectorFilter(ctx context.Context, req interface{}, rsp interface{}, next filter.ClientHandleFunc) error {
	msg := codec.Message(ctx)
	opts := OptionsFromContext(ctx)
	if IsOptionsImmutable(ctx) { // Check if options are immutable.
		// The retry plugin will start multiple goroutines to process this filter concurrently,
		// and will set the options to be immutable. Therefore, the original opts cannot be modified directly,
		// and it is necessary to clone new opts.
		opts = opts.clone()
		opts.rebuildSliceCapacity()
		ctx = contextWithOptions(ctx, opts)
	}

	// Select a node of the backend service.
	node, err := selectNode(ctx, msg, opts)
	if err != nil {
		return OptionsFromContext(ctx).fixTimeout(err)
	}
	ensureMsgRemoteAddr(msg, findFirstNonEmpty(node.Network, opts.Network), node.Address)

	// Start to process the next filter and report.
	begin := time.Now()
	err = next(ctx, req, rsp)
	cost := time.Since(begin)
	if e, ok := err.(*errs.Error); ok &&
		e.Type == errs.ErrorTypeFramework &&
		(e.Code == errs.RetClientConnectFail ||
			e.Code == errs.RetClientTimeout ||
			e.Code == errs.RetClientNetErr) {
		e.Msg = fmt.Sprintf("%s, cost:%s", e.Msg, cost)
		opts.Selector.Report(node, cost, err)
	} else if opts.shouldErrReportToSelector(err) {
		opts.Selector.Report(node, cost, err)
	} else {
		opts.Selector.Report(node, cost, nil)
	}

	// Transmits node information back to the user.
	if addr := msg.RemoteAddr(); addr != nil {
		opts.Node.set(node, addr.String(), cost)
	} else {
		opts.Node.set(node, node.Address, cost)
	}
	return err
}

// selectNode selects a backend node by selector related options and sets the msg.
func selectNode(ctx context.Context, msg codec.Msg, opts *Options) (*registry.Node, error) {
	opts.SelectOptions = append(opts.SelectOptions, selector.WithContext(ctx))
	node, err := getNode(opts)
	if err != nil {
		report.SelectNodeFail.Incr()
		return nil, err
	}

	// Update msg by node config.
	opts.LoadNodeConfig(node)
	msg.WithCalleeContainerName(node.ContainerName)
	msg.WithCalleeSetName(node.SetName)

	// Set current env info as environment message for transfer only if
	// env info from upstream service is not set.
	if msg.EnvTransfer() == "" {
		msg.WithEnvTransfer(node.EnvKey)
	}

	// If service router is disabled, env info should be cleared.
	if opts.DisableServiceRouter {
		msg.WithEnvTransfer("")
	}

	// Selector might block for a while, need to check if ctx is still available.
	if ctx.Err() == context.Canceled {
		return nil, errs.NewFrameError(errs.RetClientCanceled,
			"selector canceled after Select: "+ctx.Err().Error())
	}
	if ctx.Err() == context.DeadlineExceeded {
		return nil, errs.NewFrameError(errs.RetClientTimeout,
			"selector timeout after Select: "+ctx.Err().Error())
	}

	return node, nil
}

func getNode(opts *Options) (*registry.Node, error) {
	// Select node.
	node, err := opts.Selector.Select(opts.endpoint, opts.SelectOptions...)
	if err != nil {
		return nil, errs.NewFrameError(errs.RetClientRouteErr, "client Select: "+err.Error())
	}
	if node.Address == "" {
		return nil, errs.NewFrameError(errs.RetClientRouteErr, fmt.Sprintf("client Select: node address empty:%+v", node))
	}
	return node, nil
}

func ensureMsgRemoteAddr(msg codec.Msg, network string, address string) {
	// If RemoteAddr has already been set, just return.
	if msg.RemoteAddr() != nil {
		return
	}
	switch network {
	case "tcp", "tcp4", "tcp6", "udp", "udp4", "udp6":
		// Check if address can be parsed as an ip.
		host, _, err := net.SplitHostPort(address)
		if err != nil || net.ParseIP(host) == nil {
			return
		}
	}

	var addr net.Addr
	switch network {
	case "tcp", "tcp4", "tcp6":
		addr, _ = net.ResolveTCPAddr(network, address)
	case "udp", "udp4", "udp6":
		addr, _ = net.ResolveUDPAddr(network, address)
	case "unix":
		addr, _ = net.ResolveUnixAddr(network, address)
	default:
		addr, _ = net.ResolveTCPAddr("tcp4", address)
	}
	msg.WithRemoteAddr(addr)
}

/*
本文件是客户端代码的核心文件,需重点关注
客户端发送主要关注Invoke函数即可
*/
