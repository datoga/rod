// This file serves for the Page.Evaluate.

package rod

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/js"
	"github.com/go-rod/rod/lib/proto"
	"github.com/go-rod/rod/lib/utils"
	"github.com/ysmood/gson"
)

// EvalOptions for Page.Evaluate
type EvalOptions struct {
	// If enabled the eval result will be a plain JSON value.
	// If disabled the eval result will be a reference of a remote js object.
	ByValue bool

	AwaitPromise bool

	// ThisObj represents the "this" object in the JS
	ThisObj *proto.RuntimeRemoteObject

	// JS code to eval. It can be an expression or function definition. If it's a function definition
	// the function will be executed with the JSArgs. Such as
	//     1 + 2
	// is the same as
	//     () => 1 + 2
	// or
	//     function() {
	//         return 1 + 2
	//     }
	JS string

	// JSArgs represents the arguments in the JS if the JS is a function definition.
	// If an argument is *proto.RuntimeRemoteObject type, the corresponding remote object will be used.
	// Or it will be passed as a plain JSON value.
	JSArgs []interface{}

	// Whether execution should be treated as initiated by user in the UI.
	UserGesture bool
}

// Eval creates a EvalOptions with ByValue set to true.
//
// When an arg in the args is a *js.Function, the arg will be cached on the page's js context.
// When the arg.Name exists in the page's cache, it reuse the cache without sending the definition to the browser again.
// Useful when you need to eval a huge js expression many times.
func Eval(js string, args ...interface{}) *EvalOptions {
	return &EvalOptions{
		ByValue:      true,
		AwaitPromise: false,
		ThisObj:      nil,
		JS:           js,
		JSArgs:       args,
		UserGesture:  false,
	}
}

func evalHelper(fn *js.Function, args ...interface{}) *EvalOptions {
	return &EvalOptions{
		ByValue: true,
		JSArgs:  append([]interface{}{fn}, args...),
		JS:      `(f, ...args) => f.apply(this, args)`,
	}
}

// String interface
func (e *EvalOptions) String() string {
	fn := e.JS
	args := e.JSArgs

	paramsStr := ""
	thisStr := ""

	if e.ThisObj != nil {
		thisStr = e.ThisObj.Description
	}
	if len(args) > 0 {
		if f, ok := args[0].(*js.Function); ok {
			fn = "rod." + f.Name
			args = e.JSArgs[1:]
		}

		paramsStr = strings.Trim(mustToJSONForDev(args), "[]\r\n")
	}

	return fmt.Sprintf("%s(%s) %s", fn, paramsStr, thisStr)
}

// This set the obj as ThisObj
func (e *EvalOptions) This(obj *proto.RuntimeRemoteObject) *EvalOptions {
	e.ThisObj = obj
	return e
}

// ByObject disables ByValue.
func (e *EvalOptions) ByObject() *EvalOptions {
	e.ByValue = false
	return e
}

// ByUser enables UserGesture.
func (e *EvalOptions) ByUser() *EvalOptions {
	e.UserGesture = true
	return e
}

// ByPromise enables AwaitPromise.
func (e *EvalOptions) ByPromise() *EvalOptions {
	e.AwaitPromise = true
	return e
}

func (e *EvalOptions) formatToJSFunc() string {
	js := strings.TrimSpace(e.JS)
	js = e.JS
	if detectJSFunction(js) {
		return fmt.Sprintf(`function() { return (%s).apply(this, arguments) }`, js)
	}
	return fmt.Sprintf(`function() { return %s }`, js)
}

// Eval is just a shortcut for Page.Evaluate with AwaitPromise set true.
func (p *Page) Eval(js string, jsArgs ...interface{}) (*proto.RuntimeRemoteObject, error) {
	return p.Evaluate(Eval(js, jsArgs...).ByPromise())
}

// Evaluate js on the page.
func (p *Page) Evaluate(opts *EvalOptions) (res *proto.RuntimeRemoteObject, err error) {
	var backoff utils.Sleeper

	// js context will be invalid if a frame is reloaded or not ready, then the isNilContextErr
	// will be true, then we retry the eval again.
	for {
		res, err = p.evaluate(opts)
		if err != nil && errors.Is(err, cdp.ErrCtxNotFound) {
			if opts.ThisObj != nil {
				return nil, &ErrObjectNotFound{opts.ThisObj}
			}

			if backoff == nil {
				backoff = utils.BackoffSleeper(30*time.Millisecond, 3*time.Second, nil)
			} else {
				_ = backoff(p.ctx)
			}

			p.unsetJSCtxID()

			continue
		}
		return
	}
}

func (p *Page) evaluate(opts *EvalOptions) (*proto.RuntimeRemoteObject, error) {
	args, err := p.formatArgs(opts)
	if err != nil {
		return nil, err
	}

	req := proto.RuntimeCallFunctionOn{
		AwaitPromise:        opts.AwaitPromise,
		ReturnByValue:       opts.ByValue,
		UserGesture:         opts.UserGesture,
		FunctionDeclaration: opts.formatToJSFunc(),
		Arguments:           args,
	}

	if opts.ThisObj == nil {
		req.ObjectID, err = p.getJSCtxID()
		if err != nil {
			return nil, err
		}
	} else {
		req.ObjectID = opts.ThisObj.ObjectID
	}

	res, err := req.Call(p)
	if err != nil {
		return nil, err
	}

	if res.ExceptionDetails != nil {
		return nil, &ErrEval{res.ExceptionDetails}
	}

	return res.Result, nil
}

// Expose fn to the page's window object with the name. The exposure survives reloads.
// Call stop to unbind the fn.
func (p *Page) Expose(name string, fn func(gson.JSON) (interface{}, error)) (stop func() error, err error) {
	bind := "_" + utils.RandString(8)

	err = proto.RuntimeAddBinding{Name: bind}.Call(p)
	if err != nil {
		return
	}

	code := fmt.Sprintf(`(%s)("%s", "%s")`, js.ExposeFunc.Definition, name, bind)

	_, err = p.Evaluate(Eval(code))
	if err != nil {
		return
	}

	remove, err := p.EvalOnNewDocument(code)
	if err != nil {
		return
	}

	p, cancel := p.WithCancel()

	stop = func() error {
		defer cancel()
		err := remove()
		if err != nil {
			return err
		}
		return proto.RuntimeRemoveBinding{Name: bind}.Call(p)
	}

	go p.EachEvent(func(e *proto.RuntimeBindingCalled) {
		if e.Name == bind {
			payload := gson.NewFrom(e.Payload)
			res, err := fn(payload.Get("req"))
			code := fmt.Sprintf("(res, err) => %s(res, err)", payload.Get("cb").Str())
			_, _ = p.Evaluate(Eval(code, res, err))
		}
	})()

	return
}

func (p *Page) formatArgs(opts *EvalOptions) ([]*proto.RuntimeCallArgument, error) {
	formated := []*proto.RuntimeCallArgument{}
	for _, arg := range opts.JSArgs {
		if obj, ok := arg.(*proto.RuntimeRemoteObject); ok { // remote object
			formated = append(formated, &proto.RuntimeCallArgument{ObjectID: obj.ObjectID})
		} else if obj, ok := arg.(*js.Function); ok { // js helper
			id, err := p.ensureJSHelper(obj)
			if err != nil {
				return nil, err
			}
			formated = append(formated, &proto.RuntimeCallArgument{ObjectID: id})
		} else { // plain json data
			formated = append(formated, &proto.RuntimeCallArgument{Value: gson.New(arg)})
		}
	}

	return formated, nil
}

// Check the doc of EvalHelper
func (p *Page) ensureJSHelper(fn *js.Function) (proto.RuntimeRemoteObjectID, error) {
	jsCtxID, err := p.getJSCtxID()
	if err != nil {
		return "", err
	}

	if p.helpers == nil {
		p.helpers = map[proto.RuntimeRemoteObjectID]map[string]proto.RuntimeRemoteObjectID{}
	}

	list, ok := p.helpers[jsCtxID]
	if !ok {
		list = map[string]proto.RuntimeRemoteObjectID{}
		p.helpers[jsCtxID] = list
	}

	fns, has := list[js.Functions.Name]
	if !has {
		res, err := proto.RuntimeCallFunctionOn{
			ObjectID:            jsCtxID,
			FunctionDeclaration: js.Functions.Definition,
		}.Call(p)
		if err != nil {
			return "", err
		}
		fns = res.Result.ObjectID
		list[js.Functions.Name] = fns
	}

	id, has := list[fn.Name]
	if !has {
		for _, dep := range fn.Dependencies {
			_, err := p.ensureJSHelper(dep)
			if err != nil {
				return "", err
			}
		}

		res, err := proto.RuntimeCallFunctionOn{
			ObjectID:  jsCtxID,
			Arguments: []*proto.RuntimeCallArgument{{ObjectID: fns}},

			FunctionDeclaration: fmt.Sprintf(
				// we only need the object id, but the cdp will return the whole function string.
				// So we override the toString to reduce the overhead.
				"functions => { const f = functions.%s = %s; f.toString = () => 'fn'; return f }",
				fn.Name, fn.Definition,
			),
		}.Call(p)
		if err != nil {
			return "", err
		}

		id = res.Result.ObjectID
		list[fn.Name] = id
	}

	return id, nil
}

// Returns the page's window object, the page can be an iframe
func (p *Page) getJSCtxID() (proto.RuntimeRemoteObjectID, error) {
	p.jsCtxLock.Lock()
	defer p.jsCtxLock.Unlock()

	if *p.jsCtxID != "" {
		return *p.jsCtxID, nil
	}

	if !p.IsIframe() {
		obj, err := proto.RuntimeEvaluate{Expression: "window"}.Call(p)
		if err != nil {
			return "", err
		}

		*p.jsCtxID = obj.Result.ObjectID
		p.helpers = nil
		return *p.jsCtxID, nil
	}

	owner, err := proto.DOMGetFrameOwner{FrameID: p.FrameID}.Call(p)
	if err != nil {
		return "", err
	}

	node, err := proto.DOMDescribeNode{BackendNodeID: owner.BackendNodeID, Pierce: true}.Call(p)
	if err != nil {
		return "", err
	}

	obj, err := proto.DOMResolveNode{BackendNodeID: node.Node.ContentDocument.BackendNodeID}.Call(p)
	if err != nil {
		return "", err
	}

	delete(p.helpers, *p.jsCtxID)
	id, err := p.jsCtxIDByObjectID(obj.Object.ObjectID)
	*p.jsCtxID = id
	return *p.jsCtxID, err
}

func (p *Page) unsetJSCtxID() {
	p.jsCtxLock.Lock()
	defer p.jsCtxLock.Unlock()

	*p.jsCtxID = ""
}

func (p *Page) jsCtxIDByObjectID(id proto.RuntimeRemoteObjectID) (proto.RuntimeRemoteObjectID, error) {
	res, err := proto.RuntimeCallFunctionOn{
		ObjectID:            id,
		FunctionDeclaration: `() => window`,
	}.Call(p)
	if err != nil {
		return "", err
	}

	return res.Result.ObjectID, nil
}
