//go:generate mockgen --build_flags=--mod=mod -self_package=github.com/jensneuse/graphql-go-tools/pkg/engine/resolve -destination=resolve_mock_test.go -package=resolve . DataSource,BeforeFetchHook,AfterFetchHook

package resolve

import (
	"bytes"
	"context"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/textproto"
	"strconv"
	"sync"
	"time"
	"unicode"

	"github.com/buger/jsonparser"
	"github.com/cespare/xxhash"

	errors "golang.org/x/xerrors"

	"github.com/jensneuse/graphql-go-tools/internal/pkg/unsafebytes"
	"github.com/jensneuse/graphql-go-tools/pkg/fastbuffer"
	"github.com/jensneuse/graphql-go-tools/pkg/lexer/literal"
	"github.com/jensneuse/graphql-go-tools/pkg/pool"
)

var (
	lBrace            = []byte("{")
	rBrace            = []byte("}")
	lBrack            = []byte("[")
	rBrack            = []byte("]")
	comma             = []byte(",")
	colon             = []byte(":")
	quote             = []byte("\"")
	quotedComma       = []byte(`","`)
	null              = []byte("null")
	literalData       = []byte("data")
	literalErrors     = []byte("errors")
	literalMessage    = []byte("message")
	literalLocations  = []byte("locations")
	literalLine       = []byte("line")
	literalColumn     = []byte("column")
	literalPath       = []byte("path")
	literalExtensions = []byte("extensions")

	unableToResolveMsg = []byte("unable to resolve")
	emptyArray         = []byte("[]")
)

var (
	errNonNullableFieldValueIsNull = errors.New("non Nullable field value is null")
	errTypeNameSkipped             = errors.New("skipped because of __typename condition")
	errHeaderPathInvalid           = errors.New("invalid header path: header variables must be of this format: .request.header.{{ key }} ")

	ErrUnableToResolve = errors.New("unable to resolve operation")
)

var (
	responsePaths = [][]string{
		{"errors"},
		{"data"},
	}
	errorPaths = [][]string{
		{"message"},
		{"locations"},
		{"path"},
		{"extensions"},
	}
	entitiesPath = []string{"_entities", "[0]"}
)

const (
	rootErrorsPathIndex = 0
	rootDataPathIndex   = 1

	errorsMessagePathIndex    = 0
	errorsLocationsPathIndex  = 1
	errorsPathPathIndex       = 2
	errorsExtensionsPathIndex = 3
)

type Node interface {
	NodeKind() NodeKind
}

type NodeKind int
type FetchKind int

const (
	NodeKindObject NodeKind = iota + 1
	NodeKindEmptyObject
	NodeKindArray
	NodeKindEmptyArray
	NodeKindNull
	NodeKindString
	NodeKindBoolean
	NodeKindInteger
	NodeKindFloat

	FetchKindSingle FetchKind = iota + 1
	FetchKindParallel
)

type HookContext struct {
	CurrentPath []byte
}

type BeforeFetchHook interface {
	OnBeforeFetch(ctx HookContext, input []byte)
}

type AfterFetchHook interface {
	OnData(ctx HookContext, output []byte, singleFlight bool)
	OnError(ctx HookContext, output []byte, singleFlight bool)
}

type Context struct {
	context.Context
	Variables       []byte
	Request         Request
	pathElements    [][]byte
	patches         []patch
	usedBuffers     []*bytes.Buffer
	currentPatch    int
	maxPatch        int
	pathPrefix      []byte
	beforeFetchHook BeforeFetchHook
	afterFetchHook  AfterFetchHook
	position        Position
}

type Request struct {
	Header http.Header
}

func NewContext(ctx context.Context) *Context {
	return &Context{
		Context:      ctx,
		Variables:    make([]byte, 0, 4096),
		pathPrefix:   make([]byte, 0, 4096),
		pathElements: make([][]byte, 0, 16),
		patches:      make([]patch, 0, 48),
		usedBuffers:  make([]*bytes.Buffer, 0, 48),
		currentPatch: -1,
		maxPatch:     -1,
		position:     Position{},
	}
}

func (c *Context) Clone() Context {
	variables := make([]byte, len(c.Variables))
	copy(variables, c.Variables)
	pathPrefix := make([]byte, len(c.pathPrefix))
	copy(pathPrefix, c.pathPrefix)
	pathElements := make([][]byte, len(c.pathElements))
	for i := range pathElements {
		pathElements[i] = make([]byte, len(c.pathElements[i]))
		copy(pathElements[i], c.pathElements[i])
	}
	patches := make([]patch, len(c.patches))
	for i := range patches {
		patches[i] = patch{
			path:      make([]byte, len(c.patches[i].path)),
			extraPath: make([]byte, len(c.patches[i].extraPath)),
			data:      make([]byte, len(c.patches[i].data)),
			index:     c.patches[i].index,
		}
		copy(patches[i].path, c.patches[i].path)
		copy(patches[i].extraPath, c.patches[i].extraPath)
		copy(patches[i].data, c.patches[i].data)
	}
	return Context{
		Context:         c.Context,
		Variables:       variables,
		Request:         c.Request,
		pathElements:    pathElements,
		patches:         patches,
		usedBuffers:     make([]*bytes.Buffer, 0, 48),
		currentPatch:    c.currentPatch,
		maxPatch:        c.maxPatch,
		pathPrefix:      pathPrefix,
		beforeFetchHook: c.beforeFetchHook,
		afterFetchHook:  c.afterFetchHook,
		position:        c.position,
	}
}

func (c *Context) Free() {
	c.Context = nil
	c.Variables = c.Variables[:0]
	c.pathPrefix = c.pathPrefix[:0]
	c.pathElements = c.pathElements[:0]
	c.patches = c.patches[:0]
	for i := range c.usedBuffers {
		pool.BytesBuffer.Put(c.usedBuffers[i])
	}
	c.usedBuffers = c.usedBuffers[:0]
	c.currentPatch = -1
	c.maxPatch = -1
	c.beforeFetchHook = nil
	c.afterFetchHook = nil
	c.Request.Header = nil
	c.position = Position{}
}

func (c *Context) SetBeforeFetchHook(hook BeforeFetchHook) {
	c.beforeFetchHook = hook
}

func (c *Context) SetAfterFetchHook(hook AfterFetchHook) {
	c.afterFetchHook = hook
}

func (c *Context) setPosition(position Position) {
	c.position = position
}

func (c *Context) addPathElement(elem []byte) {
	c.pathElements = append(c.pathElements, elem)
}

func (c *Context) addIntegerPathElement(elem int) {
	b := unsafebytes.StringToBytes(strconv.Itoa(elem))
	c.pathElements = append(c.pathElements, b)
}

func (c *Context) removeLastPathElement() {
	c.pathElements = c.pathElements[:len(c.pathElements)-1]
}

func (c *Context) path() []byte {
	buf := pool.BytesBuffer.Get()
	c.usedBuffers = append(c.usedBuffers, buf)
	if len(c.pathPrefix) != 0 {
		buf.Write(c.pathPrefix)
	} else {
		buf.Write(literal.SLASH)
		buf.Write(literal.DATA)
	}
	for i := range c.pathElements {
		if i == 0 && bytes.Equal(literal.DATA, c.pathElements[0]) {
			continue
		}
		_, _ = buf.Write(literal.SLASH)
		_, _ = buf.Write(c.pathElements[i])
	}
	return buf.Bytes()
}

func (c *Context) addPatch(index int, path, extraPath, data []byte) {
	next := patch{path: path, extraPath: extraPath, data: data, index: index}
	c.patches = append(c.patches, next)
	c.maxPatch++
}

func (c *Context) popNextPatch() (patch patch, ok bool) {
	c.currentPatch++
	if c.currentPatch > c.maxPatch {
		return patch, false
	}
	return c.patches[c.currentPatch], true
}

type patch struct {
	path, extraPath, data []byte
	index                 int
}

type Fetch interface {
	FetchKind() FetchKind
}

type Fetches []Fetch

type DataSource interface {
	Load(ctx context.Context, input []byte, w io.Writer) (err error)
}

type SubscriptionDataSource interface {
	Start(ctx context.Context, input []byte, next chan<- []byte) error
}

type Resolver struct {
	EnableSingleFlightLoader bool
	resultSetPool            sync.Pool
	byteSlicesPool           sync.Pool
	waitGroupPool            sync.Pool
	bufPairPool              sync.Pool
	bufPairSlicePool         sync.Pool
	errChanPool              sync.Pool
	hash64Pool               sync.Pool
	inflightFetchPool        sync.Pool
	inflightFetchMu          sync.Mutex
	inflightFetches          map[uint64]*inflightFetch
	ctx                      context.Context
}

type inflightFetch struct {
	waitLoad sync.WaitGroup
	waitFree sync.WaitGroup
	err      error
	bufPair  BufPair
}

// New returns a new Resolver, ctx.Done() is used to cancel all active subscriptions & streams
func New(ctx context.Context) *Resolver {
	return &Resolver{
		ctx: ctx,
		resultSetPool: sync.Pool{
			New: func() interface{} {
				return &resultSet{
					buffers: make(map[int]*BufPair, 8),
				}
			},
		},
		byteSlicesPool: sync.Pool{
			New: func() interface{} {
				slice := make([][]byte, 0, 24)
				return &slice
			},
		},
		waitGroupPool: sync.Pool{
			New: func() interface{} {
				return &sync.WaitGroup{}
			},
		},
		bufPairPool: sync.Pool{
			New: func() interface{} {
				pair := BufPair{
					Data:   fastbuffer.New(),
					Errors: fastbuffer.New(),
				}
				return &pair
			},
		},
		bufPairSlicePool: sync.Pool{
			New: func() interface{} {
				slice := make([]*BufPair, 0, 24)
				return &slice
			},
		},
		errChanPool: sync.Pool{
			New: func() interface{} {
				return make(chan error, 1)
			},
		},
		hash64Pool: sync.Pool{
			New: func() interface{} {
				return xxhash.New()
			},
		},
		inflightFetchPool: sync.Pool{
			New: func() interface{} {
				return &inflightFetch{
					bufPair: BufPair{
						Data:   fastbuffer.New(),
						Errors: fastbuffer.New(),
					},
				}
			},
		},
		inflightFetches: map[uint64]*inflightFetch{},
	}
}

func (r *Resolver) resolveNode(ctx *Context, node Node, data []byte, bufPair *BufPair) (err error) {
	switch n := node.(type) {
	case *Object:
		return r.resolveObject(ctx, n, data, bufPair)
	case *Array:
		return r.resolveArray(ctx, n, data, bufPair)
	case *Null:
		if n.Defer.Enabled {
			r.preparePatch(ctx, n.Defer.PatchIndex, nil, data)
		}
		r.resolveNull(bufPair.Data)
		return
	case *String:
		return r.resolveString(n, data, bufPair)
	case *Boolean:
		return r.resolveBoolean(n, data, bufPair)
	case *Integer:
		return r.resolveInteger(n, data, bufPair)
	case *Float:
		return r.resolveFloat(n, data, bufPair)
	case *EmptyObject:
		r.resolveEmptyObject(bufPair.Data)
		return
	case *EmptyArray:
		r.resolveEmptyArray(bufPair.Data)
		return
	default:
		return
	}
}

func (r *Resolver) validateContext(ctx *Context) (err error) {
	if ctx.maxPatch != -1 || ctx.currentPatch != -1 {
		return fmt.Errorf("Context must be resetted using Free() before re-using it")
	}
	return nil
}

func (r *Resolver) extractResponse(responseData []byte, bufPair *BufPair, cfg ProcessResponseConfig) {
	if len(responseData) == 0 {
		return
	}

	if !cfg.ExtractGraphqlResponse {
		bufPair.Data.WriteBytes(responseData)
		return
	}

	jsonparser.EachKey(responseData, func(i int, bytes []byte, valueType jsonparser.ValueType, err error) {
		switch i {
		case rootErrorsPathIndex:
			_, _ = jsonparser.ArrayEach(bytes, func(value []byte, dataType jsonparser.ValueType, offset int, err error) {
				var (
					message, locations, path, extensions []byte
				)
				jsonparser.EachKey(value, func(i int, bytes []byte, valueType jsonparser.ValueType, err error) {
					switch i {
					case errorsMessagePathIndex:
						message = bytes
					case errorsLocationsPathIndex:
						locations = bytes
					case errorsPathPathIndex:
						path = bytes
					case errorsExtensionsPathIndex:
						extensions = bytes
					}
				}, errorPaths...)
				if message != nil {
					bufPair.WriteErr(message, locations, path, extensions)
				}
			})
		case rootDataPathIndex:
			if cfg.ExtractFederationEntities {
				data, _, _, _ := jsonparser.Get(bytes, entitiesPath...)
				bufPair.Data.WriteBytes(data)
				return
			}
			bufPair.Data.WriteBytes(bytes)
		}
	}, responsePaths...)
}

func (r *Resolver) ResolveGraphQLResponse(ctx *Context, response *GraphQLResponse, data []byte, writer io.Writer) (err error) {
	buf := r.getBufPair()
	defer r.freeBufPair(buf)

	responseBuf := r.getBufPair()
	defer r.freeBufPair(responseBuf)

	r.extractResponse(data, responseBuf, ProcessResponseConfig{ExtractGraphqlResponse: true})

	ignoreData := false
	err = r.resolveNode(ctx, response.Data, responseBuf.Data.Bytes(), buf)
	if err != nil {
		if !errors.Is(err, errNonNullableFieldValueIsNull) {
			return
		}
		ignoreData = true
	}
	if responseBuf.Errors.Len() > 0 {
		r.MergeBufPairErrors(responseBuf, buf)
	}

	return writeGraphqlResponse(buf, writer, ignoreData)
}

func (r *Resolver) ResolveGraphQLSubscription(ctx *Context, subscription *GraphQLSubscription, writer FlushWriter) (err error) {

	buf := r.getBufPair()
	err = subscription.Trigger.InputTemplate.Render(ctx, nil, buf.Data)
	if err != nil {
		return
	}
	rendered := buf.Data.Bytes()
	subscriptionInput := make([]byte, len(rendered))
	copy(subscriptionInput, rendered)
	r.freeBufPair(buf)

	c, cancel := context.WithCancel(ctx)
	defer cancel()
	resolverDone := r.ctx.Done()

	next := make(chan []byte)
	err = subscription.Trigger.Source.Start(c, subscriptionInput, next)
	if err != nil {
		if errors.Is(err, ErrUnableToResolve) {
			_, err = writer.Write([]byte(`{"errors":[{"message":"unable to resolve"}]}`))
			if err != nil {
				return err
			}
			writer.Flush()
			return nil
		}
		return err
	}

	for {
		select {
		case <-resolverDone:
			return nil
		default:
			data, ok := <-next
			if !ok {
				return nil
			}
			err = r.ResolveGraphQLResponse(ctx, subscription.Response, data, writer)
			if err != nil {
				return err
			}
			writer.Flush()
		}
	}
}

func (r *Resolver) ResolveGraphQLStreamingResponse(ctx *Context, response *GraphQLStreamingResponse, data []byte, writer FlushWriter) (err error) {

	if err := r.validateContext(ctx); err != nil {
		return err
	}

	err = r.ResolveGraphQLResponse(ctx, response.InitialResponse, data, writer)
	if err != nil {
		return err
	}
	writer.Flush()

	nextFlush := time.Now().Add(time.Millisecond * time.Duration(response.FlushInterval))

	buf := pool.BytesBuffer.Get()
	defer pool.BytesBuffer.Put(buf)

	buf.Write(literal.LBRACK)

	done := ctx.Context.Done()

Loop:
	for {
		select {
		case <-done:
			return
		default:
			patch, ok := ctx.popNextPatch()
			if !ok {
				break Loop
			}

			if patch.index > len(response.Patches)-1 {
				continue
			}

			if buf.Len() != 1 {
				buf.Write(literal.COMMA)
			}

			preparedPatch := response.Patches[patch.index]
			err = r.ResolveGraphQLResponsePatch(ctx, preparedPatch, patch.data, patch.path, patch.extraPath, buf)
			if err != nil {
				return err
			}

			now := time.Now()
			if now.After(nextFlush) {
				buf.Write(literal.RBRACK)
				_, err = writer.Write(buf.Bytes())
				if err != nil {
					return err
				}
				writer.Flush()
				buf.Reset()
				buf.Write(literal.LBRACK)
				nextFlush = time.Now().Add(time.Millisecond * time.Duration(response.FlushInterval))
			}
		}
	}

	if buf.Len() != 1 {
		buf.Write(literal.RBRACK)
		_, err = writer.Write(buf.Bytes())
		if err != nil {
			return err
		}
		writer.Flush()
	}

	return
}

func (r *Resolver) ResolveGraphQLResponsePatch(ctx *Context, patch *GraphQLResponsePatch, data, path, extraPath []byte, writer io.Writer) (err error) {

	buf := r.getBufPair()
	defer r.freeBufPair(buf)

	ctx.pathPrefix = append(path, extraPath...)

	if patch.Fetch != nil {
		set := r.getResultSet()
		defer r.freeResultSet(set)
		err = r.resolveFetch(ctx, patch.Fetch, data, set)
		if err != nil {
			return err
		}
		_, ok := set.buffers[0]
		if ok {
			r.MergeBufPairErrors(set.buffers[0], buf)
			data = set.buffers[0].Data.Bytes()
		}
	}

	err = r.resolveNode(ctx, patch.Value, data, buf)
	if err != nil {
		return
	}

	hasErrors := buf.Errors.Len() != 0
	hasData := buf.Data.Len() != 0

	if hasErrors {
		return
	}

	if hasData {
		if hasErrors {
			err = writeSafe(err, writer, comma)
		}
		err = writeSafe(err, writer, lBrace)
		err = writeSafe(err, writer, quote)
		err = writeSafe(err, writer, literal.OP)
		err = writeSafe(err, writer, quote)
		err = writeSafe(err, writer, colon)
		err = writeSafe(err, writer, quote)
		err = writeSafe(err, writer, patch.Operation)
		err = writeSafe(err, writer, quote)
		err = writeSafe(err, writer, comma)
		err = writeSafe(err, writer, quote)
		err = writeSafe(err, writer, literal.PATH)
		err = writeSafe(err, writer, quote)
		err = writeSafe(err, writer, colon)
		err = writeSafe(err, writer, quote)
		err = writeSafe(err, writer, path)
		err = writeSafe(err, writer, quote)
		err = writeSafe(err, writer, comma)
		err = writeSafe(err, writer, quote)
		err = writeSafe(err, writer, literal.VALUE)
		err = writeSafe(err, writer, quote)
		err = writeSafe(err, writer, colon)
		_, err = writer.Write(buf.Data.Bytes())
		err = writeSafe(err, writer, rBrace)
	}

	return
}

func (r *Resolver) resolveEmptyArray(b *fastbuffer.FastBuffer) {
	b.WriteBytes(lBrack)
	b.WriteBytes(rBrack)
}

func (r *Resolver) resolveEmptyObject(b *fastbuffer.FastBuffer) {
	b.WriteBytes(lBrace)
	b.WriteBytes(rBrace)
}

func (r *Resolver) resolveArray(ctx *Context, array *Array, data []byte, arrayBuf *BufPair) (err error) {
	if bytes.Equal(data, emptyArray) {
		r.resolveEmptyArray(arrayBuf.Data)
		return
	}

	arrayItems := r.byteSlicesPool.Get().(*[][]byte)
	defer func() {
		*arrayItems = (*arrayItems)[:0]
		r.byteSlicesPool.Put(arrayItems)
	}()

	_, err = jsonparser.ArrayEach(data, func(value []byte, dataType jsonparser.ValueType, offset int, err error) {
		*arrayItems = append(*arrayItems, value)
	}, array.Path...)

	if len(*arrayItems) == 0 {
		if !array.Nullable {
			r.resolveEmptyArray(arrayBuf.Data)
			return errNonNullableFieldValueIsNull
		}
		r.resolveNull(arrayBuf.Data)
		return nil
	}

	if array.ResolveAsynchronous && !array.Stream.Enabled {
		return r.resolveArrayAsynchronous(ctx, array, arrayItems, arrayBuf)
	}
	return r.resolveArraySynchronous(ctx, array, arrayItems, arrayBuf)
}

func (r *Resolver) resolveArraySynchronous(ctx *Context, array *Array, arrayItems *[][]byte, arrayBuf *BufPair) (err error) {

	itemBuf := r.getBufPair()
	defer r.freeBufPair(itemBuf)

	arrayBuf.Data.WriteBytes(lBrack)
	var (
		hasPreviousItem bool
		dataWritten     int
	)
	for i := range *arrayItems {

		if array.Stream.Enabled {
			if i > array.Stream.InitialBatchSize-1 {
				ctx.addIntegerPathElement(i)
				r.preparePatch(ctx, array.Stream.PatchIndex, nil, (*arrayItems)[i])
				ctx.removeLastPathElement()
				continue
			}
		}

		ctx.addIntegerPathElement(i)
		err = r.resolveNode(ctx, array.Item, (*arrayItems)[i], itemBuf)
		ctx.removeLastPathElement()
		if err != nil {
			if errors.Is(err, errNonNullableFieldValueIsNull) && array.Nullable {
				arrayBuf.Data.Reset()
				r.resolveNull(arrayBuf.Data)
				return nil
			}
			if errors.Is(err, errTypeNameSkipped) {
				err = nil
				continue
			}
			return
		}
		dataWritten += itemBuf.Data.Len()
		r.MergeBufPairs(itemBuf, arrayBuf, hasPreviousItem)
		if !hasPreviousItem && dataWritten != 0 {
			hasPreviousItem = true
		}
	}

	arrayBuf.Data.WriteBytes(rBrack)
	return
}

func (r *Resolver) resolveArrayAsynchronous(ctx *Context, array *Array, arrayItems *[][]byte, arrayBuf *BufPair) (err error) {

	arrayBuf.Data.WriteBytes(lBrack)

	bufSlice := r.getBufPairSlice()
	defer r.freeBufPairSlice(bufSlice)

	wg := r.getWaitGroup()
	defer r.freeWaitGroup(wg)

	errCh := r.getErrChan()
	defer r.freeErrChan(errCh)

	wg.Add(len(*arrayItems))

	for i := range *arrayItems {
		itemBuf := r.getBufPair()
		*bufSlice = append(*bufSlice, itemBuf)
		itemData := (*arrayItems)[i]
		cloned := ctx.Clone()
		go func(ctx Context, i int) {
			ctx.addPathElement([]byte(strconv.Itoa(i)))
			if e := r.resolveNode(&ctx, array.Item, itemData, itemBuf); e != nil && !errors.Is(e, errTypeNameSkipped) {
				select {
				case errCh <- e:
				default:
				}
			}
			ctx.Free()
			wg.Done()
		}(cloned, i)
	}

	wg.Wait()

	select {
	case err = <-errCh:
	default:
	}

	if err != nil {
		if errors.Is(err, errNonNullableFieldValueIsNull) && array.Nullable {
			arrayBuf.Data.Reset()
			r.resolveNull(arrayBuf.Data)
			return nil
		}
		return
	}

	var (
		hasPreviousItem bool
		dataWritten     int
	)
	for i := range *bufSlice {
		dataWritten += (*bufSlice)[i].Data.Len()
		r.MergeBufPairs((*bufSlice)[i], arrayBuf, hasPreviousItem)
		if !hasPreviousItem && dataWritten != 0 {
			hasPreviousItem = true
		}
	}

	arrayBuf.Data.WriteBytes(rBrack)
	return
}

func (r *Resolver) resolveInteger(integer *Integer, data []byte, integerBuf *BufPair) error {
	value, dataType, _, err := jsonparser.Get(data, integer.Path...)
	if err != nil || dataType != jsonparser.Number {
		if !integer.Nullable {
			return errNonNullableFieldValueIsNull
		}
		r.resolveNull(integerBuf.Data)
		return nil
	}
	integerBuf.Data.WriteBytes(value)
	return nil
}

func (r *Resolver) resolveFloat(floatValue *Float, data []byte, floatBuf *BufPair) error {
	value, dataType, _, err := jsonparser.Get(data, floatValue.Path...)
	if err != nil || dataType != jsonparser.Number {
		if !floatValue.Nullable {
			return errNonNullableFieldValueIsNull
		}
		r.resolveNull(floatBuf.Data)
		return nil
	}
	floatBuf.Data.WriteBytes(value)
	return nil
}

func (r *Resolver) resolveBoolean(boolean *Boolean, data []byte, booleanBuf *BufPair) error {
	value, valueType, _, err := jsonparser.Get(data, boolean.Path...)
	if err != nil || valueType != jsonparser.Boolean {
		if !boolean.Nullable {
			return errNonNullableFieldValueIsNull
		}
		r.resolveNull(booleanBuf.Data)
		return nil
	}
	booleanBuf.Data.WriteBytes(value)
	return nil
}

func (r *Resolver) resolveString(str *String, data []byte, stringBuf *BufPair) error {
	var (
		value     []byte
		valueType jsonparser.ValueType
		err       error
	)
	if len(data) != 0 && str.Path == nil {
		_, valueType, _, _ = jsonparser.Get(data)
		if valueType == jsonparser.String || unicode.IsLetter(rune(data[0])) {
			value = data
		} else if !str.Nullable {
			return errNonNullableFieldValueIsNull
		} else {
			r.resolveNull(stringBuf.Data)
			return nil
		}
	}
	if value == nil {
		value, valueType, _, err = jsonparser.Get(data, str.Path...)
		if err != nil || valueType != jsonparser.String {
			if !str.Nullable {
				return errNonNullableFieldValueIsNull
			}
			r.resolveNull(stringBuf.Data)
			return nil
		}
	}
	if value == nil && !str.Nullable {
		return errNonNullableFieldValueIsNull
	}
	stringBuf.Data.WriteBytes(quote)
	stringBuf.Data.WriteBytes(value)
	stringBuf.Data.WriteBytes(quote)
	return nil
}

func (r *Resolver) preparePatch(ctx *Context, patchIndex int, extraPath, data []byte) {
	buf := pool.BytesBuffer.Get()
	ctx.usedBuffers = append(ctx.usedBuffers, buf)
	_, _ = buf.Write(data)
	path, data := ctx.path(), buf.Bytes()
	ctx.addPatch(patchIndex, path, extraPath, data)
}

func (r *Resolver) resolveNull(b *fastbuffer.FastBuffer) {
	b.WriteBytes(null)
}

func (r *Resolver) addResolveError(ctx *Context, objectBuf *BufPair) {
	locations, path := pool.BytesBuffer.Get(), pool.BytesBuffer.Get()
	defer pool.BytesBuffer.Put(locations)
	defer pool.BytesBuffer.Put(path)

	var pathBytes []byte

	locations.Write(lBrack)
	locations.Write(lBrace)
	locations.Write(quote)
	locations.Write(literalLine)
	locations.Write(quote)
	locations.Write(colon)
	locations.Write([]byte(strconv.Itoa(int(ctx.position.Line))))
	locations.Write(comma)
	locations.Write(quote)
	locations.Write(literalColumn)
	locations.Write(quote)
	locations.Write(colon)
	locations.Write([]byte(strconv.Itoa(int(ctx.position.Column))))
	locations.Write(rBrace)
	locations.Write(rBrack)

	if len(ctx.pathElements) > 0 {
		path.Write(lBrack)
		path.Write(quote)
		path.Write(bytes.Join(ctx.pathElements, quotedComma))
		path.Write(quote)
		path.Write(rBrack)

		pathBytes = path.Bytes()
	}

	objectBuf.WriteErr(unableToResolveMsg, locations.Bytes(), pathBytes, nil)
}

func (r *Resolver) resolveObject(ctx *Context, object *Object, data []byte, objectBuf *BufPair) (err error) {
	if len(object.Path) != 0 {
		data, _, _, _ = jsonparser.Get(data, object.Path...)

		if len(data) == 0 {
			if object.Nullable {
				r.resolveNull(objectBuf.Data)
				return
			}

			r.addResolveError(ctx, objectBuf)
			return errNonNullableFieldValueIsNull
		}
	}

	var set *resultSet
	if object.Fetch != nil {
		set = r.getResultSet()
		defer r.freeResultSet(set)
		err = r.resolveFetch(ctx, object.Fetch, data, set)
		if err != nil {
			return
		}
		for i := range set.buffers {
			r.MergeBufPairErrors(set.buffers[i], objectBuf)
		}
	}

	fieldBuf := r.getBufPair()
	defer r.freeBufPair(fieldBuf)

	typeNameSkip := false
	first := true
	for i := range object.Fields {

		var fieldData []byte
		if set != nil && object.Fields[i].HasBuffer {
			buffer, ok := set.buffers[object.Fields[i].BufferID]
			if ok {
				fieldData = buffer.Data.Bytes()
			}
		} else {
			fieldData = data
		}

		if object.Fields[i].OnTypeName != nil {
			typeName, _, _, _ := jsonparser.Get(fieldData, "__typename")
			if !bytes.Equal(typeName, object.Fields[i].OnTypeName) {
				typeNameSkip = true
				continue
			}
		}

		if first {
			objectBuf.Data.WriteBytes(lBrace)
			first = false
		} else {
			objectBuf.Data.WriteBytes(comma)
		}
		objectBuf.Data.WriteBytes(quote)
		objectBuf.Data.WriteBytes(object.Fields[i].Name)
		objectBuf.Data.WriteBytes(quote)
		objectBuf.Data.WriteBytes(colon)
		ctx.addPathElement(object.Fields[i].Name)
		ctx.setPosition(object.Fields[i].Position)
		err = r.resolveNode(ctx, object.Fields[i].Value, fieldData, fieldBuf)
		ctx.removeLastPathElement()
		if err != nil {
			if errors.Is(err, errTypeNameSkipped) {
				objectBuf.Data.Reset()
				r.resolveEmptyObject(objectBuf.Data)
				return nil
			}
			if errors.Is(err, errNonNullableFieldValueIsNull) {
				objectBuf.Data.Reset()
				r.MergeBufPairErrors(fieldBuf, objectBuf)

				if object.Nullable {
					r.resolveNull(objectBuf.Data)
					return nil
				}

				// if fied is of object type than we should not add resolve error here
				if _, ok := object.Fields[i].Value.(*Object); !ok {
					r.addResolveError(ctx, objectBuf)
				}
			}

			return
		}
		r.MergeBufPairs(fieldBuf, objectBuf, false)
	}
	if first {
		if typeNameSkip {
			return errTypeNameSkipped
		}
		if !object.Nullable {
			r.addResolveError(ctx, objectBuf)
			return errNonNullableFieldValueIsNull
		}
		r.resolveNull(objectBuf.Data)
		return
	}
	objectBuf.Data.WriteBytes(rBrace)
	return
}

func (r *Resolver) freeResultSet(set *resultSet) {
	for i := range set.buffers {
		set.buffers[i].Reset()
		r.bufPairPool.Put(set.buffers[i])
		delete(set.buffers, i)
	}
	r.resultSetPool.Put(set)
}

func (r *Resolver) resolveFetch(ctx *Context, fetch Fetch, data []byte, set *resultSet) (err error) {
	switch f := fetch.(type) {
	case *SingleFetch:
		preparedInput := r.getBufPair()
		defer r.freeBufPair(preparedInput)
		err = r.prepareSingleFetch(ctx, f, data, set, preparedInput.Data)
		if err != nil {
			return err
		}
		err = r.resolveSingleFetch(ctx, f, preparedInput.Data, set.buffers[f.BufferId])
	case *ParallelFetch:
		preparedInputs := r.getBufPairSlice()
		defer r.freeBufPairSlice(preparedInputs)
		for i := range f.Fetches {
			preparedInput := r.getBufPair()
			err = r.prepareSingleFetch(ctx, f.Fetches[i], data, set, preparedInput.Data)
			if err != nil {
				return err
			}
			*preparedInputs = append(*preparedInputs, preparedInput)
		}
		wg := r.getWaitGroup()
		defer r.freeWaitGroup(wg)
		for i := range f.Fetches {
			preparedInput := (*preparedInputs)[i]
			singleFetch := f.Fetches[i]
			buf := set.buffers[f.Fetches[i].BufferId]
			wg.Add(1)
			go func(s *SingleFetch, buf *BufPair) {
				_ = r.resolveSingleFetch(ctx, s, preparedInput.Data, buf)
				wg.Done()
			}(singleFetch, buf)
		}
		wg.Wait()
	}
	return
}

func (r *Resolver) prepareSingleFetch(ctx *Context, fetch *SingleFetch, data []byte, set *resultSet, preparedInput *fastbuffer.FastBuffer) (err error) {
	err = fetch.InputTemplate.Render(ctx, data, preparedInput)
	buf := r.getBufPair()
	set.buffers[fetch.BufferId] = buf
	return
}

func (r *Resolver) resolveSingleFetch(ctx *Context, fetch *SingleFetch, preparedInput *fastbuffer.FastBuffer, buf *BufPair) (err error) {
	dataBuf := pool.BytesBuffer.Get()
	defer pool.BytesBuffer.Put(dataBuf)

	if ctx.beforeFetchHook != nil {
		ctx.beforeFetchHook.OnBeforeFetch(r.hookCtx(ctx), preparedInput.Bytes())
	}

	if !r.EnableSingleFlightLoader || fetch.DisallowSingleFlight {
		err = fetch.DataSource.Load(ctx.Context, preparedInput.Bytes(), dataBuf)
		r.extractResponse(dataBuf.Bytes(), buf, fetch.ProcessResponseConfig)
		if ctx.afterFetchHook != nil {
			if buf.HasData() {
				ctx.afterFetchHook.OnData(r.hookCtx(ctx), buf.Data.Bytes(), false)
			}
			if buf.HasErrors() {
				ctx.afterFetchHook.OnError(r.hookCtx(ctx), buf.Errors.Bytes(), false)
			}
		}
		return
	}

	hash64 := r.getHash64()
	_, _ = hash64.Write(fetch.DataSourceIdentifier)
	_, _ = hash64.Write(preparedInput.Bytes())
	fetchID := hash64.Sum64()
	r.putHash64(hash64)

	r.inflightFetchMu.Lock()
	inflight, ok := r.inflightFetches[fetchID]
	if ok {
		inflight.waitFree.Add(1)
		defer inflight.waitFree.Done()
		r.inflightFetchMu.Unlock()
		inflight.waitLoad.Wait()
		if inflight.bufPair.HasData() {
			if ctx.afterFetchHook != nil {
				ctx.afterFetchHook.OnData(r.hookCtx(ctx), inflight.bufPair.Data.Bytes(), true)
			}
			buf.Data.WriteBytes(inflight.bufPair.Data.Bytes())
		}
		if inflight.bufPair.HasErrors() {
			if ctx.afterFetchHook != nil {
				ctx.afterFetchHook.OnError(r.hookCtx(ctx), inflight.bufPair.Errors.Bytes(), true)
			}
			buf.Errors.WriteBytes(inflight.bufPair.Errors.Bytes())
		}
		return inflight.err
	}

	inflight = r.getInflightFetch()
	inflight.waitLoad.Add(1)
	r.inflightFetches[fetchID] = inflight

	r.inflightFetchMu.Unlock()

	err = fetch.DataSource.Load(ctx.Context, preparedInput.Bytes(), dataBuf)
	r.extractResponse(dataBuf.Bytes(), &inflight.bufPair, fetch.ProcessResponseConfig)
	inflight.err = err

	if inflight.bufPair.HasData() {
		if ctx.afterFetchHook != nil {
			ctx.afterFetchHook.OnData(r.hookCtx(ctx), inflight.bufPair.Data.Bytes(), false)
		}
		buf.Data.WriteBytes(inflight.bufPair.Data.Bytes())
	}

	if inflight.bufPair.HasErrors() {
		if ctx.afterFetchHook != nil {
			ctx.afterFetchHook.OnError(r.hookCtx(ctx), inflight.bufPair.Errors.Bytes(), true)
		}
		buf.Errors.WriteBytes(inflight.bufPair.Errors.Bytes())
	}

	inflight.waitLoad.Done()

	r.inflightFetchMu.Lock()
	delete(r.inflightFetches, fetchID)
	r.inflightFetchMu.Unlock()

	go func() {
		inflight.waitFree.Wait()
		r.freeInflightFetch(inflight)
	}()

	return
}

func (r *Resolver) hookCtx(ctx *Context) HookContext {
	return HookContext{
		CurrentPath: ctx.path(),
	}
}

type Object struct {
	Nullable bool
	Path     []string
	Fields   []*Field
	Fetch    Fetch
}

func (_ *Object) NodeKind() NodeKind {
	return NodeKindObject
}

type EmptyObject struct{}

func (_ *EmptyObject) NodeKind() NodeKind {
	return NodeKindEmptyObject
}

type EmptyArray struct{}

func (_ *EmptyArray) NodeKind() NodeKind {
	return NodeKindEmptyArray
}

type Field struct {
	Name       []byte
	Value      Node
	Position   Position
	Defer      *DeferField
	Stream     *StreamField
	HasBuffer  bool
	BufferID   int
	OnTypeName []byte
}

type Position struct {
	Line   uint32
	Column uint32
}

type StreamField struct {
	InitialBatchSize int
}

type DeferField struct{}

type Null struct {
	Defer Defer
}

type Defer struct {
	Enabled    bool
	PatchIndex int
}

func (_ *Null) NodeKind() NodeKind {
	return NodeKindNull
}

type resultSet struct {
	buffers map[int]*BufPair
}

type SingleFetch struct {
	BufferId   int
	Input      string
	DataSource DataSource
	Variables  Variables
	// DisallowSingleFlight is used for write operations like mutations, POST, DELETE etc. to disable singleFlight
	// By default SingleFlight for fetches is disabled and needs to be enabled on the Resolver first
	// If the resolver allows SingleFlight it's up the each individual DataSource Planner to decide whether an Operation
	// should be allowed to use SingleFlight
	DisallowSingleFlight  bool
	InputTemplate         InputTemplate
	DataSourceIdentifier  []byte
	ProcessResponseConfig ProcessResponseConfig
}

type ProcessResponseConfig struct {
	ExtractGraphqlResponse    bool
	ExtractFederationEntities bool
}

type InputTemplate struct {
	Segments []TemplateSegment
}

func (i *InputTemplate) Render(ctx *Context, data []byte, preparedInput *fastbuffer.FastBuffer) (err error) {
	for j := range i.Segments {
		switch i.Segments[j].SegmentType {
		case StaticSegmentType:
			preparedInput.WriteBytes(i.Segments[j].Data)
		case VariableSegmentType:
			switch i.Segments[j].VariableSource {
			case VariableSourceObject:
				err = i.renderObjectVariable(data, i.Segments[j].VariableSourcePath, preparedInput)
			case VariableSourceContext:
				err = i.renderContextVariable(ctx, i.Segments[j].VariableSourcePath, i.Segments[j].RenderAsGraphQLValue, preparedInput)
			case VariableSourceRequestHeader:
				err = i.renderHeaderVariable(ctx, i.Segments[j].VariableSourcePath, preparedInput)
			default:
				err = fmt.Errorf("InputTemplate.Render: cannot resolve variable of kind: %d", i.Segments[j].VariableSource)
			}
			if err != nil {
				return err
			}
		}
	}
	return
}

func (i *InputTemplate) renderObjectVariable(data []byte, path []string, preparedInput *fastbuffer.FastBuffer) error {
	value, _, _, err := jsonparser.Get(data, path...)
	if err != nil {
		return err
	}
	preparedInput.WriteBytes(value)
	return nil
}

func (i *InputTemplate) renderContextVariable(ctx *Context, path []string, renderAsGraphQLValue bool, preparedInput *fastbuffer.FastBuffer) error {
	value, valueType, _, err := jsonparser.Get(ctx.Variables, path...)
	if err != nil {
		return err
	}
	if !renderAsGraphQLValue {
		preparedInput.WriteBytes(value)
		return nil
	}
	return i.renderGraphQLValue(value, valueType, preparedInput)
}

func (i *InputTemplate) renderGraphQLValue(data []byte, valueType jsonparser.ValueType, buf *fastbuffer.FastBuffer) (err error) {
	switch valueType {
	case jsonparser.String:
		buf.WriteBytes(literal.BACKSLASH)
		buf.WriteBytes(literal.QUOTE)
		buf.WriteBytes(data)
		buf.WriteBytes(literal.BACKSLASH)
		buf.WriteBytes(literal.QUOTE)
	case jsonparser.Object:
		buf.WriteBytes(literal.LBRACE)
		first := true
		err = jsonparser.ObjectEach(data, func(key []byte, value []byte, dataType jsonparser.ValueType, offset int) error {
			if !first {
				buf.WriteBytes(literal.COMMA)
			} else {
				first = false
			}
			buf.WriteBytes(key)
			buf.WriteBytes(literal.COLON)
			return i.renderGraphQLValue(value, dataType, buf)
		})
		if err != nil {
			return err
		}
		buf.WriteBytes(literal.RBRACE)
	case jsonparser.Null:
		buf.WriteBytes(literal.NULL)
	case jsonparser.Boolean:
		buf.WriteBytes(data)
	case jsonparser.Array:
		buf.WriteBytes(literal.LBRACK)
		first := true
		var arrayErr error
		_, err = jsonparser.ArrayEach(data, func(value []byte, dataType jsonparser.ValueType, offset int, err error) {
			if !first {
				buf.WriteBytes(literal.COMMA)
			} else {
				first = false
			}
			arrayErr = i.renderGraphQLValue(value, dataType, buf)
		})
		if arrayErr != nil {
			return arrayErr
		}
		if err != nil {
			return err
		}
		buf.WriteBytes(literal.RBRACK)
	case jsonparser.Number:
		buf.WriteBytes(data)
	}
	return
}

func (i *InputTemplate) renderHeaderVariable(ctx *Context, path []string, preparedInput *fastbuffer.FastBuffer) error {
	if len(path) != 1 {
		return errHeaderPathInvalid
	}
	// Header.Values is available from go 1.14
	// value := ctx.Request.Header.Values(path[0])
	// could be simplified once go 1.12 support will be dropped
	canonicalName := textproto.CanonicalMIMEHeaderKey(path[0])
	value := ctx.Request.Header[canonicalName]
	if len(value) == 0 {
		return nil
	}
	if len(value) == 1 {
		preparedInput.WriteString(value[0])
		return nil
	}
	for j := range value {
		if j != 0 {
			preparedInput.WriteBytes(literal.COMMA)
		}
		preparedInput.WriteString(value[j])
	}
	return nil
}

type SegmentType int
type VariableSource int

const (
	StaticSegmentType SegmentType = iota + 1
	VariableSegmentType

	VariableSourceObject VariableSource = iota + 1
	VariableSourceContext
	VariableSourceRequestHeader
)

type TemplateSegment struct {
	SegmentType          SegmentType
	Data                 []byte
	VariableSource       VariableSource
	VariableSourcePath   []string
	RenderAsGraphQLValue bool
}

func (_ *SingleFetch) FetchKind() FetchKind {
	return FetchKindSingle
}

type ParallelFetch struct {
	Fetches []*SingleFetch
}

func (_ *ParallelFetch) FetchKind() FetchKind {
	return FetchKindParallel
}

type String struct {
	Path     []string
	Nullable bool
}

func (_ *String) NodeKind() NodeKind {
	return NodeKindString
}

type Boolean struct {
	Path     []string
	Nullable bool
}

func (_ *Boolean) NodeKind() NodeKind {
	return NodeKindBoolean
}

type Float struct {
	Path     []string
	Nullable bool
}

func (_ *Float) NodeKind() NodeKind {
	return NodeKindFloat
}

type Integer struct {
	Path     []string
	Nullable bool
}

func (_ *Integer) NodeKind() NodeKind {
	return NodeKindInteger
}

type Array struct {
	Path                []string
	Nullable            bool
	ResolveAsynchronous bool
	Item                Node
	Stream              Stream
}

type Stream struct {
	Enabled          bool
	InitialBatchSize int
	PatchIndex       int
}

func (_ *Array) NodeKind() NodeKind {
	return NodeKindArray
}

type Variable interface {
	VariableKind() VariableKind
	Equals(another Variable) bool
	TemplateSegment() TemplateSegment
}

type Variables []Variable

func NewVariables(variables ...Variable) Variables {
	return variables
}

const (
	variablePrefixSuffix = "$$"
	quotes               = "\""
)

func (v *Variables) AddVariable(variable Variable, quoteValue bool) (name string, exists bool) {
	index := -1
	for i := range *v {
		if (*v)[i].Equals(variable) {
			index = i
			exists = true
			break
		}
	}
	if index == -1 {
		*v = append(*v, variable)
		index = len(*v) - 1
	}
	i := strconv.Itoa(index)
	name = variablePrefixSuffix + i + variablePrefixSuffix
	if quoteValue {
		name = quotes + name + quotes
	}
	return
}

type VariableKind int

const (
	VariableKindContext VariableKind = iota + 1
	VariableKindObject
	VariableKindHeader
)

type ContextVariable struct {
	Path                 []string
	RenderAsGraphQLValue bool
}

func (c *ContextVariable) TemplateSegment() TemplateSegment {
	return TemplateSegment{
		SegmentType:          VariableSegmentType,
		VariableSource:       VariableSourceContext,
		VariableSourcePath:   c.Path,
		RenderAsGraphQLValue: c.RenderAsGraphQLValue,
	}
}

func (c *ContextVariable) Equals(another Variable) bool {
	if another == nil {
		return false
	}
	if another.VariableKind() != c.VariableKind() {
		return false
	}
	anotherContextVariable := another.(*ContextVariable)
	if len(c.Path) != len(anotherContextVariable.Path) {
		return false
	}
	for i := range c.Path {
		if c.Path[i] != anotherContextVariable.Path[i] {
			return false
		}
	}
	return true
}

func (_ *ContextVariable) VariableKind() VariableKind {
	return VariableKindContext
}

type ObjectVariable struct {
	Path []string
}

func (o *ObjectVariable) TemplateSegment() TemplateSegment {
	return TemplateSegment{
		SegmentType:        VariableSegmentType,
		VariableSource:     VariableSourceObject,
		VariableSourcePath: o.Path,
	}
}

func (o *ObjectVariable) Equals(another Variable) bool {
	if another == nil {
		return false
	}
	if another.VariableKind() != o.VariableKind() {
		return false
	}
	anotherObjectVariable := another.(*ObjectVariable)
	if len(o.Path) != len(anotherObjectVariable.Path) {
		return false
	}
	for i := range o.Path {
		if o.Path[i] != anotherObjectVariable.Path[i] {
			return false
		}
	}
	return true
}

func (o *ObjectVariable) VariableKind() VariableKind {
	return VariableKindObject
}

type HeaderVariable struct {
	Path []string
}

func (h *HeaderVariable) TemplateSegment() TemplateSegment {
	return TemplateSegment{
		SegmentType:        VariableSegmentType,
		VariableSource:     VariableSourceRequestHeader,
		VariableSourcePath: h.Path,
	}
}

func (h *HeaderVariable) VariableKind() VariableKind {
	return VariableKindHeader
}

func (h *HeaderVariable) Equals(another Variable) bool {
	if another == nil {
		return false
	}
	if another.VariableKind() != h.VariableKind() {
		return false
	}
	anotherHeaderVariable := another.(*HeaderVariable)
	if len(h.Path) != len(anotherHeaderVariable.Path) {
		return false
	}
	for i := range h.Path {
		if h.Path[i] != anotherHeaderVariable.Path[i] {
			return false
		}
	}
	return true
}

type GraphQLSubscription struct {
	Trigger  GraphQLSubscriptionTrigger
	Response *GraphQLResponse
}

type GraphQLSubscriptionTrigger struct {
	Input         []byte
	InputTemplate InputTemplate
	Variables     Variables
	Source        SubscriptionDataSource
}

type FlushWriter interface {
	io.Writer
	Flush()
}

type GraphQLResponse struct {
	Data Node
}

type GraphQLStreamingResponse struct {
	InitialResponse *GraphQLResponse
	Patches         []*GraphQLResponsePatch
	FlushInterval   int64
}

type GraphQLResponsePatch struct {
	Value     Node
	Fetch     Fetch
	Operation []byte
}

type BufPair struct {
	Data   *fastbuffer.FastBuffer
	Errors *fastbuffer.FastBuffer
}

func NewBufPair() *BufPair {
	return &BufPair{
		Data:   fastbuffer.New(),
		Errors: fastbuffer.New(),
	}
}

func (b *BufPair) HasData() bool {
	return b.Data.Len() != 0
}

func (b *BufPair) HasErrors() bool {
	return b.Errors.Len() != 0
}

func (b *BufPair) Reset() {
	b.Data.Reset()
	b.Errors.Reset()
}

func (b *BufPair) writeErrors(data []byte) {
	b.Errors.WriteBytes(data)
}

func (b *BufPair) WriteErr(message, locations, path, extensions []byte) {
	if b.HasErrors() {
		b.writeErrors(comma)
	}
	b.writeErrors(lBrace)
	b.writeErrors(quote)
	b.writeErrors(literalMessage)
	b.writeErrors(quote)
	b.writeErrors(colon)
	b.writeErrors(quote)
	b.writeErrors(message)
	b.writeErrors(quote)

	if locations != nil {
		b.writeErrors(comma)
		b.writeErrors(quote)
		b.writeErrors(literalLocations)
		b.writeErrors(quote)
		b.writeErrors(colon)
		b.writeErrors(locations)
	}

	if path != nil {
		b.writeErrors(comma)
		b.writeErrors(quote)
		b.writeErrors(literalPath)
		b.writeErrors(quote)
		b.writeErrors(colon)
		b.writeErrors(path)
	}

	if extensions != nil {
		b.writeErrors(comma)
		b.writeErrors(quote)
		b.writeErrors(literalExtensions)
		b.writeErrors(quote)
		b.writeErrors(colon)
		b.writeErrors(extensions)
	}

	b.writeErrors(rBrace)
}

func (r *Resolver) MergeBufPairs(from, to *BufPair, prefixDataWithComma bool) {
	r.MergeBufPairData(from, to, prefixDataWithComma)
	r.MergeBufPairErrors(from, to)
}

func (r *Resolver) MergeBufPairData(from, to *BufPair, prefixDataWithComma bool) {
	if !from.HasData() {
		return
	}
	if prefixDataWithComma {
		to.Data.WriteBytes(comma)
	}
	to.Data.WriteBytes(from.Data.Bytes())
	from.Data.Reset()
}

func (r *Resolver) MergeBufPairErrors(from, to *BufPair) {
	if !from.HasErrors() {
		return
	}
	if to.HasErrors() {
		to.Errors.WriteBytes(comma)
	}
	to.Errors.WriteBytes(from.Errors.Bytes())
	from.Errors.Reset()
}

func (r *Resolver) freeBufPair(pair *BufPair) {
	pair.Data.Reset()
	pair.Errors.Reset()
	r.bufPairPool.Put(pair)
}

func (r *Resolver) getResultSet() *resultSet {
	return r.resultSetPool.Get().(*resultSet)
}

func (r *Resolver) getBufPair() *BufPair {
	return r.bufPairPool.Get().(*BufPair)
}

func (r *Resolver) getBufPairSlice() *[]*BufPair {
	return r.bufPairSlicePool.Get().(*[]*BufPair)
}

func (r *Resolver) freeBufPairSlice(slice *[]*BufPair) {
	for i := range *slice {
		r.freeBufPair((*slice)[i])
	}
	*slice = (*slice)[:0]
	r.bufPairSlicePool.Put(slice)
}

func (r *Resolver) getErrChan() chan error {
	return r.errChanPool.Get().(chan error)
}

func (r *Resolver) freeErrChan(ch chan error) {
	r.errChanPool.Put(ch)
}

func (r *Resolver) getWaitGroup() *sync.WaitGroup {
	return r.waitGroupPool.Get().(*sync.WaitGroup)
}

func (r *Resolver) freeWaitGroup(wg *sync.WaitGroup) {
	r.waitGroupPool.Put(wg)
}

func (r *Resolver) getInflightFetch() *inflightFetch {
	return r.inflightFetchPool.Get().(*inflightFetch)
}

func (r *Resolver) freeInflightFetch(f *inflightFetch) {
	f.bufPair.Data.Reset()
	f.bufPair.Errors.Reset()
	f.err = nil
	r.inflightFetchPool.Put(f)
}

func (r *Resolver) getHash64() hash.Hash64 {
	return r.hash64Pool.Get().(hash.Hash64)
}

func (r *Resolver) putHash64(h hash.Hash64) {
	h.Reset()
	r.hash64Pool.Put(h)
}

func writeGraphqlResponse(buf *BufPair, writer io.Writer, ignoreData bool) (err error) {
	hasErrors := buf.Errors.Len() != 0
	hasData := buf.Data.Len() != 0 && !ignoreData

	err = writeSafe(err, writer, lBrace)

	if hasErrors {
		err = writeSafe(err, writer, quote)
		err = writeSafe(err, writer, literalErrors)
		err = writeSafe(err, writer, quote)
		err = writeSafe(err, writer, colon)
		err = writeSafe(err, writer, lBrack)
		err = writeSafe(err, writer, buf.Errors.Bytes())
		err = writeSafe(err, writer, rBrack)
		err = writeSafe(err, writer, comma)
	}

	err = writeSafe(err, writer, quote)
	err = writeSafe(err, writer, literalData)
	err = writeSafe(err, writer, quote)
	err = writeSafe(err, writer, colon)

	if hasData {
		_, err = writer.Write(buf.Data.Bytes())
	} else {
		err = writeSafe(err, writer, literal.NULL)
	}
	err = writeSafe(err, writer, rBrace)

	return err
}

func writeSafe(err error, writer io.Writer, data []byte) error {
	if err != nil {
		return err
	}
	_, err = writer.Write(data)
	return err
}
