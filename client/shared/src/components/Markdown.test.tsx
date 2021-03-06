import React from 'react'
import renderer from 'react-test-renderer'

import { Markdown } from './Markdown'

describe('Markdown', () => {
    it('renders', () => {
        const component = renderer.create(<Markdown dangerousInnerHTML="hello" />)
        expect(component.toJSON()).toMatchSnapshot()
    })
})
