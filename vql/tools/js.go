package tools

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime/debug"

	"github.com/Velocidex/ordereddict"
	"github.com/lithdew/quickjs"
	vql_subsystem "www.velocidex.com/golang/velociraptor/vql"
	"www.velocidex.com/golang/vfilter"
)

var halt = errors.New("Halt")

func logIfPanic(scope *vfilter.Scope) {
	err := recover()
	if err == halt {
		return
	}

	if err != nil {
		scope.Log("PANIC %v: %v\n", err, string(debug.Stack()))
	}
}

func check(err error) {
	if err != nil {
		var evalErr *quickjs.Error
		if errors.As(err, &evalErr) {
			fmt.Println(evalErr.Cause)
			fmt.Println(evalErr.Stack)
		}
		panic(err)
	}
}

func interfaceToValue(val reflect.Value, vm *quickjs.Context) quickjs.Value {
	switch val.Kind() {
	case reflect.Bool:
		return vm.Bool(val.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return vm.Int64(val.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return vm.BigUint64(val.Uint())
	case reflect.String:
		return vm.String(val.String())
	case reflect.Slice:
		arr := vm.Array()
		for i := 0; i < val.Len(); i++ {
			arr.Set(string(i), interfaceToValue(val.Index(i), vm))
		}
		return arr
	case reflect.Float32, reflect.Float64:
		return vm.Float64(val.Float())
	}

	return vm.Null()
}

func valueToInterface(val quickjs.Value) interface{} {
	defer val.Free()

	if val.IsFunction() {
		return vfilter.Null{}
	}

	if val.IsNumber() {
		return val.Int64()
	}

	if val.IsBigInt() {
		return val.BigInt()
	}
	if val.IsBigFloat() {
		return val.BigFloat()
	}
	if val.IsBigDecimal() {
		return val.BigFloat()
	}
	if val.IsBool() {
		return val.Bool()
	}
	if val.IsNull() {
		return vfilter.Null{}
	}
	if val.IsUndefined() {
		return vfilter.Null{}
	}
	if val.IsString() {
		return val.String()
	}

	if val.IsArray() {

		names, err := val.PropertyNames()
		check(err)

		res := make([]interface{}, 0)
		for _, name := range names {
			if name.Atom.String() != "length" {
				val := val.GetByAtom(name.Atom)
				res = append(res, valueToInterface(val))
			}

		}
		return res

	}

	if val.IsObject() {
		names, err := val.PropertyNames()
		check(err)

		res := make(map[string]vfilter.Any)
		for _, name := range names {
			val := val.GetByAtom(name.Atom)
			res[name.Atom.String()] = valueToInterface(val)
		}
		return res
	}

	return vfilter.Null{}
}

type JSCompileArgs struct {
	JS  string `vfilter:"required,field=js,doc=The body of the javascript code."`
	Key string `vfilter:"optional,field=key,doc=If set use this key to cache the JS VM."`
}

type JSCompile struct{}

func getVM(ctx context.Context,
	scope *vfilter.Scope,
	key string) *quickjs.Context {
	if key == "" {
		key = "__jscontext"
	}

	context, ok := vql_subsystem.CacheGet(scope, key).(*quickjs.Context)
	if !ok {
		runtime := quickjs.NewRuntime()
		context = runtime.NewContext()
		globals := context.Globals()

		vql_info := func(ctx *quickjs.Context, this quickjs.Value, args []quickjs.Value) quickjs.Value {
			scope.Log("JSVM: %+v", args[0].String())
			return ctx.Null()
		}

		console := context.Object()
		console.SetByAtom(context.Atom("log"), context.Function(vql_info))
		globals.Set("console", console)

		go func() {
			<-ctx.Done()
			context.Free()
			runtime.Free()
		}()
		vql_subsystem.CacheSet(scope, key, context)
	}

	return context
}

func (self *JSCompile) Call(ctx context.Context,
	scope *vfilter.Scope,
	args *ordereddict.Dict) vfilter.Any {
	arg := &JSCompileArgs{}
	err := vfilter.ExtractArgs(scope, args, arg)
	if err != nil {
		scope.Log("js: %s", err.Error())
		return vfilter.Null{}
	}

	defer logIfPanic(scope)

	vm := getVM(ctx, scope, arg.Key)

	result, err := vm.Eval(arg.JS)
	check(err)

	return valueToInterface(result)
}

func (self JSCompile) Info(scope *vfilter.Scope,
	type_map *vfilter.TypeMap) *vfilter.FunctionInfo {
	return &vfilter.FunctionInfo{
		Name:    "js",
		Doc:     "Compile and run javascript code.",
		ArgType: type_map.AddType(scope, &JSCompileArgs{}),
	}
}

type JSSetArgs struct {
	Var   string      `vfilter:"required,field=var,doc=The variable to set inside the JS VM."`
	Value vfilter.Any `vfilter:"required,field=value,doc=The value to set inside the VM."`
	Key   string      `vfilter:"optional,field=key,doc=If set use this key to cache the JS VM."`
}

type JSSet struct{}

func (self *JSSet) Call(ctx context.Context,
	scope *vfilter.Scope,
	args *ordereddict.Dict) vfilter.Any {
	arg := &JSSetArgs{}
	err := vfilter.ExtractArgs(scope, args, arg)
	if err != nil {
		scope.Log("js_set: %s", err.Error())
		return vfilter.Null{}
	}

	defer logIfPanic(scope)

	vm := getVM(ctx, scope, arg.Key)

	vm.Globals().SetByAtom(vm.Atom(arg.Var), interfaceToValue(reflect.ValueOf(arg.Value), vm))

	return vfilter.Null{}
}

func (self JSSet) Info(scope *vfilter.Scope,
	type_map *vfilter.TypeMap) *vfilter.FunctionInfo {
	return &vfilter.FunctionInfo{
		Name:    "js_set",
		Doc:     "Set a variables value in the JS VM.",
		ArgType: type_map.AddType(scope, &JSSetArgs{}),
	}
}

type JSGetArgs struct {
	Var string `vfilter:"required,field=var,doc=The variable to get from the JS VM."`
	Key string `vfilter:"optional,field=key,doc=If set use this key to cache the JS VM."`
}

type JSGet struct{}

func (self *JSGet) Call(ctx context.Context,
	scope *vfilter.Scope,
	args *ordereddict.Dict) vfilter.Any {
	arg := &JSGetArgs{}
	err := vfilter.ExtractArgs(scope, args, arg)
	if err != nil {
		scope.Log("js_get: %s", err.Error())
		return vfilter.Null{}
	}

	defer logIfPanic(scope)

	vm := getVM(ctx, scope, arg.Key)

	val := vm.Globals().GetByAtom(vm.Atom(arg.Var))

	return valueToInterface(val)
}

func (self JSGet) Info(scope *vfilter.Scope,
	type_map *vfilter.TypeMap) *vfilter.FunctionInfo {
	return &vfilter.FunctionInfo{
		Name:    "js_get",
		Doc:     "Get a variable's value from the JS VM.",
		ArgType: type_map.AddType(scope, &JSGetArgs{}),
	}
}

func init() {
	vql_subsystem.RegisterFunction(&JSCompile{})
	vql_subsystem.RegisterFunction(&JSSet{})
	vql_subsystem.RegisterFunction(&JSGet{})
}
