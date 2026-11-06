import { renderToString } from 'react-dom/server';
import { App } from './App';

export function render() {
  return renderToString(<App />);
}

export { renderContent };
import { ContentPage, type ContentPageProps } from './ContentPage';
function renderContent(props: ContentPageProps) {
  return renderToString(<ContentPage {...props} />);
}
