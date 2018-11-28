// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package rlp

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
)

//sync包提供了基本的同步，如互斥锁。除了Once和WaitGroup类型，大部分都是适用于低水平程序线程，高水平的同步使用channel通信更好一些。
var (
	//Mutex是一个互斥锁，可以创建为其他结构体的字段
	typeCacheMutex sync.RWMutex					//读写锁，用来在多线程的时候保护typeCache这个Map
	//核心数据结构，保存了类型->编解码器函数
	//map有两种初始化的方式，map[string]string{}或make(map[string]string)
	typeCache      = make(map[typekey]*typeinfo)			//map[typekey]*typeinfo，相当于map<typekey,*typeinfo>
)

type typeinfo struct {			//存储了编码器和解码器函数
	decoder
	writer
}

// represents struct tags
type tags struct {
	// rlp:"nil" controls whether empty input results in a nil pointer.   控制空输入是否导致nil指针。
	nilOK bool
	// rlp:"tail" controls whether this field swallows additional list  控制此字段是否吞噬其他列表元素
	// elements. It can only be set for the last field, which must be  它只能设置为最后一个字段，这是必须是slice类型
	// of slice type.
	tail bool
	// rlp:"-" ignores fields. 忽略此字段
	ignored bool
}

type typekey struct {						//type表示给struct取一个别名
	reflect.Type
	// the key must include the struct tags because they
	// might generate a different decoder.
	tags
}

type decoder func(*Stream, reflect.Value) error

//定义一个函数，别名叫writer
type writer func(reflect.Value, *encbuf) error

//获取对应类型的typeinfo(包含编码器和解码器函数)
func cachedTypeInfo(typ reflect.Type, tags tags) (*typeinfo, error) {
	typeCacheMutex.RLock()			//加读锁来保护，
	//传入类型到一个map中，然后返回编解码函数
	info := typeCache[typekey{typ, tags}]			//:=是短变量声明, 定义一个或多个变量并根据它们的初始值为这些变量赋予适当类型
	typeCacheMutex.RUnlock()
	//如果成功获取到信息，那么就返回
	if info != nil {			//nil代表指针、通道、函数、接口、映射或切片的零值
		return info, nil
	}
	// not in the cache, need to generate info for this type.
	//否则加写锁 调用cachedTypeInfo1函数创建并返回， 这里需要注意的是在多线程环境下有可能多个线程同时调用到这个地方，所以当你进入
	// cachedTypeInfo1方法的时候需要判断一下是否已经被别的线程先创建成功了。
	typeCacheMutex.Lock()
	defer typeCacheMutex.Unlock()			//这个要等本函数完全执行完之后才执行这行，defer延迟执行
	return cachedTypeInfo1(typ, tags)
}

//根据传入的类型，创建并返回对应的typeinfo(包含编码器和解码器函数)
func cachedTypeInfo1(typ reflect.Type, tags tags) (*typeinfo, error) {
	key := typekey{typ, tags}
	info := typeCache[key]				//先去map中取该类型对应的value(编解码函数)。info是*typeinfo类型，即typeinfo类型的指针
	if info != nil {
		// another goroutine got the write lock first
		return info, nil			// 其他的线程可能已经创建成功了， 那么我们直接获取到信息然后返回
	}
	// put a dummy value into the cache before generating.
	// if the generator tries to lookup itself, it will get
	// the dummy value and won't call itself recursively.
	typeCache[key] = new(typeinfo)
	//genTypeInfo：生成对应类型的编解码器函数。
	info, err := genTypeInfo(typ, tags)
	if err != nil {						//创建失败
		// remove the dummy value if the generator fails
		delete(typeCache, key)			//删除map中对应key的键值对
		return nil, err
	}
	*typeCache[key] = *info    //info是指向typeinfo类型的指针，*info把这个typeinfo类型变量取出
	return typeCache[key], err			//这个err其实位nil
}

type field struct {
	index int
	info  *typeinfo
}

//structFields函数遍历所有的字段，然后针对每一个字段调用cachedTypeInfo1
func structFields(typ reflect.Type) (fields []field, err error) {
	for i := 0; i < typ.NumField(); i++ {				//NumField返回struct类型的字段计数。
		if f := typ.Field(i); f.PkgPath == "" { // exported  //f.PkgPath == "" 这个判断针对的是所有导出的字段， 所谓的导出的字段就是说以大写字母开头命令的字段。
			tags, err := parseStructTag(typ, i)			//parseStructTag解析标签tags
			if err != nil {
				return nil, err
			}
			if tags.ignored {
				continue
			}
			info, err := cachedTypeInfo1(f.Type, tags)			//针对每一个字段调用cachedTypeInfo1
			if err != nil {
				return nil, err
			}
			fields = append(fields, field{i, info})
		}
	}
	return fields, nil
}

func parseStructTag(typ reflect.Type, fi int) (tags, error) {
	f := typ.Field(fi)
	var ts tags
	for _, t := range strings.Split(f.Tag.Get("rlp"), ",") {
		switch t = strings.TrimSpace(t); t {
		case "":
		case "-":
			ts.ignored = true
		case "nil":
			ts.nilOK = true
		case "tail":
			ts.tail = true
			if fi != typ.NumField()-1 {
				return ts, fmt.Errorf(`rlp: invalid struct tag "tail" for %v.%s (must be on last field)`, typ, f.Name)
			}
			if f.Type.Kind() != reflect.Slice {
				return ts, fmt.Errorf(`rlp: invalid struct tag "tail" for %v.%s (field type is not slice)`, typ, f.Name)
			}
		default:
			return ts, fmt.Errorf("rlp: unknown struct tag %q on %v.%s", t, typ, f.Name)
		}
	}
	return ts, nil
}
//生成对应类型的编解码器函数。
func genTypeInfo(typ reflect.Type, tags tags) (info *typeinfo, err error) {
	info = new(typeinfo)
	//info.decoder, err = makeDecoder(typ, tags)是赋值，不是布尔表达式
	if info.decoder, err = makeDecoder(typ, tags); err != nil {		//创建解码器
		return nil, err				//创建失败的返回值
	}
	if info.writer, err = makeWriter(typ, tags); err != nil {			//创建编码器
		return nil, err
	}
	//两个都创建成功时，返回info（包括info.decoder、info.writer）
	return info, nil
}

func isUint(k reflect.Kind) bool {
	return k >= reflect.Uint && k <= reflect.Uintptr
}
