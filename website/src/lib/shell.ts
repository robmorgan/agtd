/*
 * Shell highlighting for the docs' Terminal blocks, at build time. Shiki
 * tokenises with its bash grammar; the theme maps scopes to the --term-syn-*
 * variables in tokens.css, so the colours stay in the design system and
 * follow the terminal, which is dark in both themes.
 */
import { createHighlighter, type ThemeRegistration } from 'shiki';

const theme: ThemeRegistration = {
	name: 'agtd-term',
	type: 'dark',
	colors: { 'editor.foreground': 'var(--term-text)', 'editor.background': 'transparent' },
	tokenColors: [
		{ settings: { foreground: 'var(--term-text)' } },
		{ scope: ['entity.name.command', 'support.function.builtin'], settings: { foreground: 'var(--term-syn-command)' } },
		{ scope: ['constant.other.option'], settings: { foreground: 'var(--term-syn-flag)' } },
		{ scope: ['string.quoted', 'punctuation.definition.string'], settings: { foreground: 'var(--term-syn-string)' } },
		{ scope: ['variable', 'punctuation.definition.variable'], settings: { foreground: 'var(--term-syn-flag)' } },
		{
			scope: ['keyword.operator', 'punctuation.separator', 'punctuation.definition.subshell', 'comment', 'punctuation.definition.comment'],
			settings: { foreground: 'var(--term-muted)' },
		},
	],
};

const highlighter = await createHighlighter({ themes: [theme], langs: ['bash'] });

export interface Token {
	content: string;
	color?: string;
}

/** One command as coloured runs of text. */
export function shellTokens(command: string): Token[] {
	const { tokens } = highlighter.codeToTokens(command, { lang: 'bash', theme: 'agtd-term' });
	return tokens.flat().map((t) => ({ content: t.content, color: t.color }));
}
