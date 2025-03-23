// curl -vs http://127.0.0.1:8080/v1/chat/completions --header "Content-Type: application/json" --header "Authorization: Bearer a7364737e221432389d5fd8cf662bd64" -d "{\"model\": \"4bd107bff85941239e27b1509eccfe98\", \"stream\": true, \"messages\": [{\"role\": \"user\", \"content\": \"1+1=\"}], \"user\": \"fc4c8c42-70e7-4ddd-af92-738afff3b7c4\"}"

// hproxy1.exe -listen :8080 -target https://wishub-x1.ctyun.cn -req-body "\"stream\":?true::\"stream\": false" -resp-body "\"messages\"::\"delta\""

package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

type Config struct {
	listenPort string
	targetURL  string
	reqHeader  []string
	reqBody    []string
	respHeader []string
	respBody   []string
}

type RegexReplacement struct {
	Pattern     *regexp.Regexp
	Replacement string
	Count       int // -1 表示替换所有匹配项
	HeaderKey   string
}

var (
	config           Config
	reqHeaderRules   []RegexReplacement
	reqBodyRules     []RegexReplacement
	respHeaderRules  []RegexReplacement
	respBodyRules    []RegexReplacement
)


func customUsage() {
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, "简单HTTP反向代理\n用法：%s [参数...]\n", os.Args[0])
	flag.PrintDefaults()  
	fmt.Fprintf(os.Stderr, "\n示例:\n  %s -listen :8080 -target https://wishub-x1.ctyun.cn -req-body \"\\\"stream\\\":?true::\\\"stream\\\": false\" -resp-body \"\\\"message\\\"::\\\"delta\\\"\"\n", os.Args[0])
}


func main() {
	
	flag.Usage = customUsage
	quiet := flag.Bool("quiet", false, "不输出详细信息")
	
	flag.StringVar(&config.listenPort, "listen", ":8080", "本地HTTP侦听端口 (e.g., :8080)")
	flag.StringVar(&config.targetURL, "target", "", "反向代理目标 (e.g., https://wishub-x1.ctyun.cn)")
	flag.Func("req-header", "请求头替换规则 (key:pattern:replacement)", func(s string) error {
		config.reqHeader = append(config.reqHeader, s)
		return nil
	})
	flag.Func("req-body", "请求体替换规则 (pattern::replacement)", func(s string) error {
		config.reqBody = append(config.reqBody, s)
		return nil
	})
	flag.Func("resp-header", "应答头替换规则 (key::pattern::replacement)", func(s string) error {
		config.respHeader = append(config.respHeader, s)
		return nil
	})
	flag.Func("resp-body", "应答体替换规则 (pattern::replacement)", func(s string) error {
		config.respBody = append(config.respBody, s)
		return nil
	})

	flag.Parse()

	if config.targetURL == "" {
		log.Fatal("反向代理目标不能为空")
	}

	// 解析目标URL
	target, err := url.Parse(config.targetURL)
	if err != nil {
		log.Fatalf("目标URL格式不对: %v", err)
	}

	// 解析替换规则
	parseRules(config.reqHeader, &reqHeaderRules, true)
	parseRules(config.reqBody, &reqBodyRules, false)
	parseRules(config.respHeader, &respHeaderRules, true)
	parseRules(config.respBody, &respBodyRules, false)

	// 创建反向代理
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			if !*quiet {
				log.Println("=== 收到的请求头 ===")
				req.Header.Write(os.Stdout)
				fmt.Println()
			}

			if len(reqHeaderRules) >0 {
				modifyHeaders(req.Header, reqHeaderRules)
				if !*quiet {
					fmt.Println("=== 修改后的请求头 ===")
					req.Header.Write(os.Stdout)
					fmt.Println()
				}
			} else {
				if !*quiet {
					fmt.Println("=== 请求头无需修改 ===")
					fmt.Println()
				}
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if !*quiet {
				log.Println("=== 收到的应答头 ===")
				resp.Header.Write(os.Stdout)
				fmt.Println()
			}
			if len(respHeaderRules) >0 {
				modifyHeaders(resp.Header, respHeaderRules)
			
				if !*quiet {
					fmt.Println("=== 修改后的应答头 ===")
					resp.Header.Write(os.Stdout)
					fmt.Println()
				}
			} else {
				if !*quiet {
					fmt.Println("=== 应答头无需修改 ===")
					fmt.Println()
				}
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}
			resp.Body.Close()

			if !*quiet {
				log.Println("=== 收到的应答体 ===")
				fmt.Println(string(body))
				fmt.Println()
			} else {
				log.Println("=== 收到应答 ===")
			}
			modifiedBody:=body
			if len(respBodyRules) >0 {
				modifiedBody = modifyContent(body, respBodyRules)
				if !*quiet {
					fmt.Println("=== 修改后的应答体 ===")
					fmt.Println(string(modifiedBody))
					fmt.Println()
				}
			} else {
				if !*quiet {
					fmt.Println("=== 应答体无需修改 ===")
					fmt.Println()
				}
			}

			resp.Body = io.NopCloser(bytes.NewReader(modifiedBody))
			resp.ContentLength = int64(len(modifiedBody))
			return nil
		},
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
		},
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "请求体出错", http.StatusInternalServerError)
			return
		}
		r.Body.Close()
		if !*quiet {
			log.Println("=== 收到的请求体 ===")
			fmt.Println(string(body))
			fmt.Println()
		} else {
			log.Println("=== 收到请求 ===")
		}
		modifiedBody:=body
		if len(reqBodyRules) >0 {
			modifiedBody = modifyContent(body, reqBodyRules)
			if !*quiet {
				fmt.Println("=== 修改后的请求体 ===")
				fmt.Println(string(modifiedBody))
				fmt.Println()
			}
		} else {
			if !*quiet {
				fmt.Println("=== 请求体无需修改 ===")
				fmt.Println()
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(modifiedBody))
		r.ContentLength = int64(len(modifiedBody))

		proxy.ServeHTTP(w, r)
	})

	log.Printf("HTTP侦听端口 %s, 转发到 %s", config.listenPort, config.targetURL)
	log.Fatal(http.ListenAndServe(config.listenPort, handler))
}

// 解析替换规则，支持通配符和替换次数
func parseRules(rules []string, target *[]RegexReplacement, isHeader bool) {
	for _, rule := range rules {
		parts := strings.SplitN(rule, "::", 4)
		var pattern, replacement, headerKey string
		var count int = -1

		if isHeader {
			if len(parts) < 3 {
				log.Printf("头部改写规则格式不对: %s (例 key:pattern:replacement)", rule)
				continue
			}
			headerKey = parts[0]
			pattern = parts[1]
			replacement = parts[2]
			if len(parts) == 4 && parts[3] != "" {
				var err error
				count, err = strconv.Atoi(parts[3])
				if err != nil {
					log.Printf("替换次数字段格式不对 %s: %v", rule, err)
					continue
				}
			}
		} else {
			if len(parts) < 2 {
				log.Printf("内容改写规则格式不对: %s (例 pattern:replacement)", rule)
				continue
			}
			pattern = parts[0]
			replacement = parts[1]
			if len(parts) == 3 && parts[2] != "" {
				var err error
				count, err = strconv.Atoi(parts[2])
				if err != nil {
					log.Printf("替换次数字段格式不对 %s: %v", rule, err)
					continue
				}
			}
		}

		// 将通配符转换为正则表达式
		pattern = regexp.QuoteMeta(pattern)
		pattern = strings.ReplaceAll(pattern, `\*`, "[.\\s]*")
		pattern = strings.ReplaceAll(pattern, `\?`, "[.\\s]")
		//pattern = strings.ReplaceAll(pattern, `\?`, ".")

		regex, err := regexp.Compile(pattern)
		if err != nil {
			log.Printf("字符串匹配异常 %s: %v", pattern, err)
			continue
		}

		*target = append(*target, RegexReplacement{
			Pattern:     regex,
			Replacement: replacement,
			Count:       count,
			HeaderKey:   headerKey,
		})
	}
}

// 修改headers
func modifyHeaders(headers http.Header, rules []RegexReplacement) {
	for _, rule := range rules {
		if rule.HeaderKey == "" {
			continue
		}
		values, ok := headers[rule.HeaderKey]
		if !ok {
			continue
		}
		for i, value := range values {
			if rule.Count == -1 {
				values[i] = rule.Pattern.ReplaceAllString(value, rule.Replacement)
			} else {
				remaining := rule.Count
				values[i] = rule.Pattern.ReplaceAllStringFunc(value, func(s string) string {
					if remaining > 0 {
						remaining--
						return rule.Replacement
					}
					return s
				})
			}
		}
		headers[rule.HeaderKey] = values
	}
}

// 修改内容
func modifyContent(content []byte, rules []RegexReplacement) []byte {
	result := content
	for _, rule := range rules {
		if rule.Count == -1 {
			result = rule.Pattern.ReplaceAll(result, []byte(rule.Replacement))
		} else {
			remaining := rule.Count
			result = rule.Pattern.ReplaceAllFunc(result, func(b []byte) []byte {
				if remaining > 0 {
					remaining--
					return []byte(rule.Replacement)
				}
				return b
			})
		}
	}
	return result
}