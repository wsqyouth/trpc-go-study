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

package codec

import "trpc.group/trpc-go/trpc-go/codec"

// IsValidSerializationType checks whether t is a valid serialization type.
// 有效的序列号格式枚举集合, 业务自定义场景下需要使用1000以上的枚举值
func IsValidSerializationType(t int) bool {
	const minValidSerializationType = codec.SerializationTypePB
	return t >= minValidSerializationType
}
