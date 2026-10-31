package chatgpt

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"time"
)

//go:embed response.js
var responseProbeJS string

type responseState struct {
	IDs            []string `json:"ids"`
	LastID         string   `json:"lastID"`
	StopBtn        bool     `json:"stopBtn"`
	ContinueBtn    bool     `json:"continueBtn"`
	ErrorText      bool     `json:"errorText"`
	Last           string   `json:"lastAssistant"`
	Reasoning      string   `json:"reasoning"`
	Fresh          bool     `json:"fresh"`
	Complete       bool     `json:"complete"`
	SourceRequired bool     `json:"sourceRequired"`
	SourceVerified bool     `json:"-"` // exact completed final readback, not a DOM inference
}

type emptyAnswerWatch struct {
	Started  time.Time
	Since    time.Time
	SeenStop bool
}

func (w *emptyAnswerWatch) expired(st responseState, now time.Time) bool {
	if st.StopBtn {
		w.SeenStop = true
		w.Since = time.Time{}
		return false
	}
	if st.Last != "" || st.ContinueBtn {
		w.Since = time.Time{}
		return false
	}
	if w.SeenStop || st.Complete || now.Sub(w.Started) >= 30*time.Second {
		if w.Since.IsZero() {
			w.Since = now
		}
		return now.Sub(w.Since) >= 15*time.Second
	}
	return false
}

// responseBaseline captures DOM identities only. A previous answer's source
// availability must never gate submission of a new turn.
func responseBaseline(tab context.Context) ([]string, error) {
	var ids []string
	err := chromedp.Run(tab, chromedp.Evaluate("("+responseProbeJS+"([])).ids", &ids))
	return ids, err
}

func responseProbe(tab context.Context, baseline []string) (responseState, error) {
	if baseline == nil {
		baseline = []string{}
	}
	b, _ := json.Marshal(baseline)
	var st responseState
	err := chromedp.Run(tab, chromedp.Evaluate(responseProbeJS+"("+string(b)+")", &st))
	if err == nil && st.Fresh && st.Complete && (st.Last == "" || st.SourceRequired) && !st.StopBtn && !st.ContinueBtn && !st.ErrorText {
		// The observed DIL renderer can consume literal protocol tags and render
		// null. Read only that fresh DOM identity, never a backend latest branch.
		// Re-probe inside the readback to fence a changed user/assistant boundary.
		probe := responseProbeJS + "(" + string(b) + ")"
		var recovered struct {
			Text   string `json:"text"`
			Reason string `json:"reason"`
		}
		expected, _ := json.Marshal(st.LastID)
		// Observed source fetches take about fourteen seconds even for a
		// finished answer. Still bounded by the caller/recovery deadline.
		read, cancel := context.WithTimeout(tab, 20*time.Second)
		defer cancel()
		js := "(async () => {const recheck = () => " + probe + "; const st = recheck(); let reason='identity'; const sourceFailure = r => {reason=r;return '';}; const text=await(async()=>{if (st.lastID !== " + string(expected) + ") return '';" + nullDILWireJS + "})(); return {text,reason};})()"
		if rerr := chromedp.Run(read, chromedp.Evaluate(js, &recovered, chromedp.EvalAsValue, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); rerr != nil {
			return st, fmt.Errorf("completed source readback stage=cdp: %w", rerr)
		} else if recovered.Text != "" {
			st.Last = recovered.Text
			st.SourceVerified = true
		} else if st.SourceRequired {
			return st, fmt.Errorf("completed protocol source could not be verified against its DOM identity; rendered arguments are not safe (reason=%s)", recovered.Reason)
		}
	}
	return st, err
}

// Completed protocol responses use their exact DOM-bound source: either the
// captured null-DIL failure or visible protocol in DIL and non-DIL renderers.
// Ordinary Markdown remains DOM-only; all schema validation stays in the adapter.
// Neither the model's renderer code nor its textual tool call is run.
const nullDILWireJS = `
 // The caller may have observed an empty renderer just before ACK hydrates.
 // Verify that exact acknowledgement rather than return the stale empty
 // snapshot. Other ordinary prose still cannot enter the source fallback.
 if (!st.fresh || !st.complete || (st.lastAssistant && !st.sourceRequired && st.lastAssistant.trim() !== 'ACK') || st.stopBtn || st.continueBtn || st.errorText) return '';
 const id = st.lastID;
 if (!/^[a-zA-Z0-9-]+$/.test(id)) return '';
  const dilRoot = [...document.querySelectorAll('[data-dil-message-id]')].find(e => e.getAttribute('data-dil-message-id') === id);
  // The current alternate renderer has no DIL root. Its assistant heading
  // lives inside an owner carrying the exact same ID responseProbe selected.
  // Its rendered body can also be empty despite a completed protocol source.
  // Read only this selected identity; ordinary prose is never substituted.
  const ownerID = e => (e?.getAttribute('data-chatgpt-search-message-ids') || e?.getAttribute('data-message-id') || '').trim().split(/\s+/).at(-1);
  const heading = [...document.querySelectorAll('h4')].filter(e => e.getAttribute('data-conversation-role') === 'assistant' || e.textContent.trim() === 'ChatGPT said:').find(e => ownerID(e.closest('[data-chatgpt-search-message-ids], [data-message-id]')) === id);
  const root = dilRoot || (heading && heading.closest('[data-chatgpt-search-message-ids], [data-message-id]'));
  if (!root) return '';
  const users = [...document.querySelectorAll('h4')].filter(e => e.textContent.trim() === 'You said:');
  const user = users.at(-1);
  const userID = h => ownerID(h?.nextElementSibling?.closest('[data-chatgpt-search-message-ids], [data-message-id]') || h?.closest('[data-chatgpt-search-message-ids], [data-message-id]'));
  const uid = userID(user);
 if (!uid || !(user.compareDocumentPosition(root) & Node.DOCUMENT_POSITION_FOLLOWING)) return '';
 const thread = location.pathname.split('/c/')[1];
 if (!thread) return '';
 try {
   // Retry transport/auth availability only. Keep the original DOM root,
   // assistant, user and thread bound across every read; never select latest.
   const controller = new AbortController();
   const timer = setTimeout(() => controller.abort(), 18000);
   let mapping, reason = 'unavailable';
   try {
     for (let attempt = 0; attempt < 3; attempt++) {
       try {
         const authResponse = await fetch('/api/auth/session', {signal:controller.signal});
         if (!authResponse.ok) { reason='auth-http'; if(authResponse.status < 500 && authResponse.status !== 429) break; }
         else {
           const auth = await authResponse.json();
           if (!auth.accessToken) reason='auth-unavailable';
           else {
             const r = await fetch('/backend-api/conversation/' + encodeURIComponent(thread), {headers:{Authorization:'Bearer ' + auth.accessToken},signal:controller.signal});
             if (r.ok) { mapping=(await r.json()).mapping; break; }
             reason='conversation-http';
             if (r.status < 500 && r.status !== 429 && r.status !== 404) break;
           }
         }
       } catch (_) { reason=controller.signal.aborted?'timeout':'transport'; }
       if (controller.signal.aborted) break;
       await new Promise(resolve => setTimeout(resolve, 250));
     }
   } finally { clearTimeout(timer); }
   if (!mapping) return typeof sourceFailure === 'function' ? sourceFailure(reason) : '';
   const node = mapping?.[id], m = node?.message, dil = m?.metadata?.model_dil_v2;
   if (m?.id !== id || m.author?.role !== 'assistant' || m.channel !== 'final' || m.recipient !== 'all' ||
       m.status !== 'finished_successfully' || m.end_turn !== true || m.metadata?.is_complete !== true ||
        m.content?.content_type !== 'text' || m.content.parts?.length !== 1 || typeof m.content.parts[0] !== 'string') return '';
    const text = m.content.parts[0].trim();
    const acknowledgement = text === 'ACK';
      // ACK has no executable arguments. Its exact source can be recovered
      // across empty/visible/DIL hydration under the same final+ancestry
      // checks. Hidden executable protocol retains its narrower null-DIL
      // diagnostic requirements unless the renderer requests source verification.
      if (!st.sourceRequired && dilRoot && !acknowledgement && (dilRoot.childNodes.length !== 0 || !dil || dil.fallbackMarkdown !== '' || !/return null;/.test(dil.code || '') ||
        !dil.recoveryDiagnostics?.some(d => d.code === 'invalid_tag_syntax' && d.action === 'recovered_parse'))) return '';
   // Follow only the selected DOM node's declared ancestry to the latest DOM
   // user; no sibling, current_node or latest-message selection is allowed.
   let parent = node.parent, bound = false;
   for (let i = 0; parent && i < 32; i++) {
     const p = mapping[parent];
     if (!p || p.message?.id !== parent) return '';
     if (p.message.author?.role === 'user') { bound = parent === uid; break; }
     parent = p.parent;
   }
    // Do not reintroduce standalone empty escape artifacts via source fallback.
    // Refuse rather than rewriting source: JSON strings inside exact wrappers
    // (e.g. {"value":"<escape>"}) remain byte-for-byte unchanged.
    if (/(?:^|\n)[ \t]*(?:<escape>\s*<\/escape>[ \t]*)+(?=\n|$)/.test(text)) return '';
     // History imports can finish with an exact ACK source and an empty
     // renderer too. Do not expand this to arbitrary unrendered prose.
     if (!bound || (!acknowledgement && !((st.sourceRequired || !dilRoot) ? /<\/?opencode(?:\\)?_bridge(?:\\)?_call\b/ : /^<opencode(?:\\)?_bridge(?:\\)?_call\b/).test(text))) return '';
    // React can finish hydrating this SAME final answer while source fetches
    // await network. Visual text/sourceRequired can change without identity
    // changing. Fence on immutable source IDs, completion and user ancestry,
    // not equality of the renderer's temporary text representation.
    const after = recheck();
    const afterUser = [...document.querySelectorAll('h4')].filter(e => e.textContent.trim() === 'You said:').at(-1);
     if (location.pathname.split('/c/')[1] !== thread || !root.isConnected || after.lastID !== id || !after.fresh || !after.complete || after.stopBtn || after.continueBtn || after.errorText || userID(afterUser) !== uid || !afterUser || !(afterUser.compareDocumentPosition(root) & Node.DOCUMENT_POSITION_FOLLOWING)) return '';
   return text;
 } catch (_) { return typeof sourceFailure === 'function' ? sourceFailure('exception') : ''; }
`
