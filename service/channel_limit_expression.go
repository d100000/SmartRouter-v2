package service

import (
	"math"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/parser"
)

type channelCostInterval struct{ low, high float64 }

type channelCostBounder struct {
	prompt, output *int64
	request        billingexpr.RequestInput
	used           map[string]bool
	nodes          int
}

// channelLimitExpressionUpperQuota encloses every possible token allocation
// and both branches of a condition. Sampling endpoints is insufficient for
// decreasing prices, mixed cache conditions, or interior price peaks.
// Unsupported operations fail closed; the billing expression itself is never
// rewritten and remains authoritative for settlement.
func channelLimitExpressionUpperQuota(snapshot *billingexpr.BillingSnapshot, prompt, output *int64, request billingexpr.RequestInput) (float64, error) {
	if snapshot == nil || snapshot.TaskUsageBilling || snapshot.ExprVersion != 0 && snapshot.ExprVersion != 1 ||
		math.IsNaN(snapshot.QuotaPerUnit) || math.IsInf(snapshot.QuotaPerUnit, 0) || snapshot.QuotaPerUnit <= 0 {
		return 0, model.ErrChannelLimitAccounting
	}
	if _, err := billingexpr.CompileFromCache(snapshot.ExprString); err != nil {
		return 0, model.ErrChannelLimitAccounting
	}
	_, body := billingexpr.ParseExprVersion(snapshot.ExprString)
	tree, err := parser.Parse(body)
	if err != nil {
		return 0, model.ErrChannelLimitAccounting
	}
	bounder := channelCostBounder{prompt: prompt, output: output, request: request, used: billingexpr.UsedVars(snapshot.ExprString)}
	bound, err := bounder.interval(tree.Node, 0)
	if err != nil || bound.high < 0 {
		return 0, model.ErrChannelLimitAccounting
	}
	quota := bound.high / 1_000_000 * snapshot.QuotaPerUnit
	if _, err := common.QuotaRoundStrict(quota); err != nil {
		return 0, model.ErrChannelLimitAccounting
	}
	return quota, nil
}

func (bounder *channelCostBounder) interval(node ast.Node, depth int) (channelCostInterval, error) {
	bounder.nodes++
	if depth > 64 || bounder.nodes > 2048 {
		return channelCostInterval{}, model.ErrChannelLimitAccounting
	}
	var result channelCostInterval
	switch node := node.(type) {
	case *ast.IntegerNode:
		result = channelCostInterval{float64(node.Value), float64(node.Value)}
	case *ast.FloatNode:
		result = channelCostInterval{node.Value, node.Value}
	case *ast.IdentifierNode:
		var maximum *int64
		factor := 1.0
		switch node.Value {
		case "p", "len", "cr", "cc", "cc1h", "cc12h", "img", "img_cr", "ai":
			maximum = bounder.prompt
			// Unpriced long-TTL writes are normalized into p or cc with a
			// multiplier up to two, rather than counted as ordinary text.
			if (node.Value == "p" && !bounder.used["cc"] || node.Value == "cc") && (!bounder.used["cc1h"] || !bounder.used["cc12h"]) {
				factor = 2
			}
		case "c", "ao", "img_o":
			maximum = bounder.output
		case "image_count":
			// The response may correct the requested image quantity upwards.
			// The shared settlement guard bounds that quantity by MaxImageN.
			result.high = float64(dto.MaxImageN)
		default:
			return result, model.ErrChannelLimitAccounting
		}
		if node.Value != "image_count" {
			if maximum == nil || *maximum < 0 || *maximum > model.ChannelLimitMaxValue {
				return result, model.ErrChannelLimitAccounting
			}
			result.high = float64(*maximum) * factor
		}
	case *ast.UnaryNode:
		value, err := bounder.interval(node.Node, depth+1)
		if err != nil {
			return result, err
		}
		switch node.Operator {
		case "+":
			result = value
		case "-":
			result = channelCostInterval{-value.high, -value.low}
		default:
			return result, model.ErrChannelLimitAccounting
		}
	case *ast.BinaryNode:
		left, err := bounder.interval(node.Left, depth+1)
		if err != nil {
			return result, err
		}
		right, err := bounder.interval(node.Right, depth+1)
		if err != nil {
			return result, err
		}
		switch node.Operator {
		case "+":
			result = channelCostInterval{left.low + right.low, left.high + right.high}
		case "-":
			result = channelCostInterval{left.low - right.high, left.high - right.low}
		case "*":
			values := [4]float64{left.low * right.low, left.low * right.high, left.high * right.low, left.high * right.high}
			result = channelCostInterval{min(values[0], values[1], values[2], values[3]), max(values[0], values[1], values[2], values[3])}
		case "/":
			if right.low <= 0 && right.high >= 0 {
				return result, model.ErrChannelLimitAccounting
			}
			values := [4]float64{left.low / right.low, left.low / right.high, left.high / right.low, left.high / right.high}
			result = channelCostInterval{min(values[0], values[1], values[2], values[3]), max(values[0], values[1], values[2], values[3])}
		default:
			return result, model.ErrChannelLimitAccounting
		}
	case *ast.ConditionalNode:
		left, err := bounder.interval(node.Exp1, depth+1)
		if err != nil {
			return result, err
		}
		right, err := bounder.interval(node.Exp2, depth+1)
		if err != nil {
			return result, err
		}
		result = channelCostInterval{min(left.low, right.low), max(left.high, right.high)}
	case *ast.BuiltinNode:
		// The raw parser classifies names such as max as builtins; billing
		// compilation replaces them with the matching math functions.
		return bounder.interval(&ast.CallNode{Callee: &ast.IdentifierNode{Value: node.Name}, Arguments: node.Arguments}, depth+1)
	case *ast.CallNode:
		callee, ok := node.Callee.(*ast.IdentifierNode)
		if !ok {
			return result, model.ErrChannelLimitAccounting
		}
		switch callee.Value {
		case "tier":
			if len(node.Arguments) != 2 {
				return result, model.ErrChannelLimitAccounting
			}
			return bounder.interval(node.Arguments[1], depth+1)
		case "hour", "minute", "weekday", "month", "day":
			// These can change between admission and completion.
			bounds := map[string]channelCostInterval{"hour": {0, 23}, "minute": {0, 59}, "weekday": {0, 6}, "month": {1, 12}, "day": {1, 31}}
			result = bounds[callee.Value]
		case "fixed", "abs", "ceil", "floor", "min", "max":
			if len(node.Arguments) < 1 || len(node.Arguments) > 2 {
				return result, model.ErrChannelLimitAccounting
			}
			value, err := bounder.interval(node.Arguments[0], depth+1)
			if err != nil {
				return result, err
			}
			result = value
			switch callee.Value {
			case "fixed":
				result.low, result.high = value.low*1_000_000, value.high*1_000_000
			case "abs":
				result.low, result.high = min(math.Abs(value.low), math.Abs(value.high)), max(math.Abs(value.low), math.Abs(value.high))
				if value.low <= 0 && value.high >= 0 {
					result.low = 0
				}
			case "ceil":
				result.low, result.high = math.Ceil(value.low), math.Ceil(value.high)
			case "floor":
				result.low, result.high = math.Floor(value.low), math.Floor(value.high)
			case "min", "max":
				if len(node.Arguments) != 2 {
					return result, model.ErrChannelLimitAccounting
				}
				other, err := bounder.interval(node.Arguments[1], depth+1)
				if err != nil {
					return result, err
				}
				if callee.Value == "min" {
					result = channelCostInterval{min(value.low, other.low), min(value.high, other.high)}
				} else {
					result = channelCostInterval{max(value.low, other.low), max(value.high, other.high)}
				}
			}
		case "param":
			// Numeric frozen request facts are constants. No token-dependent
			// arithmetic or time probe is evaluated at one sampled value.
			if len(node.Arguments) != 1 {
				return result, model.ErrChannelLimitAccounting
			}
			if _, ok := node.Arguments[0].(*ast.StringNode); !ok {
				return result, model.ErrChannelLimitAccounting
			}
			value, _, err := billingexpr.RunExprWithRequest(node.String(), billingexpr.TokenParams{}, bounder.request)
			if err != nil {
				return result, model.ErrChannelLimitAccounting
			}
			result = channelCostInterval{value, value}
		default:
			return result, model.ErrChannelLimitAccounting
		}
	default:
		return result, model.ErrChannelLimitAccounting
	}
	if math.IsNaN(result.low) || math.IsNaN(result.high) || math.IsInf(result.low, 0) || math.IsInf(result.high, 0) || result.low > result.high {
		return channelCostInterval{}, model.ErrChannelLimitAccounting
	}
	return result, nil
}
